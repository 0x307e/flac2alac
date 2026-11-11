package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cheggaaa/pb/v3"
)

type kind int

const (
	kindCopy kind = iota
	kindConvert
)

type job struct {
	rel  string
	kind kind
}

// isFLAC is the only place extension case is decided, so .flac, .FLAC and
// .Flac cannot drift apart across the walk and the dispatch.
func isFLAC(rel string) bool {
	return strings.EqualFold(filepath.Ext(rel), ".flac")
}

func walk(in string) ([]job, error) {
	var jobs []job
	var errs []error
	err := filepath.WalkDir(in, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				fmt.Fprintf(os.Stderr, "read dir %s: %v\n", p, err)
				errs = append(errs, err)
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(in, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		k := kindCopy
		if isFLAC(rel) {
			k = kindConvert
		}
		jobs = append(jobs, job{rel: rel, kind: k})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return jobs, errors.Join(errs...)
}

// dstPath derives every output path from the relative path alone. Editing the
// full input path in place would rewrite directory names such as "My FLAC Rip".
func dstPath(out string, j job) string {
	if j.kind == kindConvert {
		return filepath.Join(out, filepath.FromSlash(strings.TrimSuffix(j.rel, filepath.Ext(j.rel)))+".m4a")
	}
	return filepath.Join(out, filepath.FromSlash(j.rel))
}

func convert(src, dst string) error {
	cmd := exec.Command("ffmpeg", "-nostdin", "-y", "-hide_banner", "-loglevel", "error",
		"-i", src, "-map", "0:a", "-c:a", "alac", dst)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// ffmpeg can leave a partial file, and an existing stale .m4a would make
		// a later run look converted when it was not.
		os.Remove(dst)
		return fmt.Errorf("%s: %v: %s", src, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	srcF, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcF.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".flac2alac-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if _, err := io.Copy(tmp, srcF); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp applies the umask, so re-apply the source mode afterwards.
	if err := os.Chmod(tmp.Name(), info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chtimes(tmp.Name(), info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func run(in, out string, jobs []job) (failed int, err error) {
	var mu sync.Mutex
	var errs []error
	record := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	ch := make(chan job)
	var wg sync.WaitGroup
	bar := pb.StartNew(len(jobs))
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for jb := range ch {
				dst := dstPath(out, jb)
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					record(fmt.Errorf("%s: %w", jb.rel, err))
					bar.Increment()
					continue
				}
				var err error
				switch jb.kind {
				case kindCopy:
					err = copyFile(filepath.Join(in, filepath.FromSlash(jb.rel)), dst)
				case kindConvert:
					err = convert(filepath.Join(in, filepath.FromSlash(jb.rel)), dst)
				default:
					err = fmt.Errorf("%s: unknown job kind %d", jb.rel, jb.kind)
				}
				if err != nil {
					record(err)
				}
				bar.Increment()
			}
		}()
	}
	for _, jb := range jobs {
		ch <- jb
	}
	close(ch)
	wg.Wait()
	bar.Finish()
	mu.Lock()
	defer mu.Unlock()
	return len(errs), errors.Join(errs...)
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: flac2alac <input-dir> <output-dir>")
		os.Exit(2)
	}
	in, out := os.Args[1], os.Args[2]

	info, err := os.Stat(in)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "input %s is not a readable directory\n", in)
		os.Exit(1)
	}
	absIn, err := filepath.Abs(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "input %s: %v\n", in, err)
		os.Exit(1)
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "output %s: %v\n", out, err)
		os.Exit(1)
	}
	rel, err := filepath.Rel(filepath.Clean(absIn), filepath.Clean(absOut))
	if err == nil && !strings.HasPrefix(rel, "..") {
		fmt.Fprintf(os.Stderr, "output %s must be outside input %s\n", out, in)
		os.Exit(1)
	}

	items, err := walk(in)
	if err != nil && items == nil {
		fmt.Fprintf(os.Stderr, "walk %s: %v\n", in, err)
		os.Exit(1)
	}

	failed, runErr := run(in, out, items)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walk %s: %v\n", in, err)
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, runErr)
	}
	if err != nil || runErr != nil {
		if failed == 0 {
			failed = 1
		}
		fmt.Fprintf(os.Stderr, "%d of %d files failed\n", failed, len(items))
		os.Exit(1)
	}
}
