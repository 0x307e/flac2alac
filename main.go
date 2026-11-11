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

const workers = 4

type job struct {
	rel  string
	kind kind
}

type kind int

const (
	kindCopy kind = iota
	kindConvert
)

func isFLAC(rel string) bool {
	return strings.EqualFold(filepath.Ext(rel), ".flac")
}

// walk returns the jobs to run and the read errors it hit on the way. A nil
// job list with an error means the walk could not start.
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

// dstPath builds the output path from the relative path alone, so a directory
// named FLAC survives instead of being edited out of the full input path.
func (j job) dstPath(out string) string {
	if j.kind == kindConvert {
		return filepath.Join(out, filepath.FromSlash(strings.TrimSuffix(j.rel, filepath.Ext(j.rel)))+".m4a")
	}
	return filepath.Join(out, filepath.FromSlash(j.rel))
}

// apply writes one job. The result lands at the final path only when it is
// complete, so a failure leaves nothing for a later run to mistake for output.
func (j job) apply(in, out string) error {
	src := filepath.Join(in, filepath.FromSlash(j.rel))
	dst := j.dstPath(out)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("%s: %w", j.rel, err)
	}
	switch j.kind {
	case kindCopy:
		return copyFile(src, dst)
	default:
		return convert(src, dst)
	}
}

func convert(src, dst string) error {
	tmp := dst + ".part.m4a"
	cmd := exec.Command("ffmpeg", "-nostdin", "-y", "-hide_banner", "-loglevel", "error",
		"-i", src, "-map", "0:a", "-c:a", "alac", tmp)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%s: %v: %s", src, err, strings.TrimSpace(stderr.String()))
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
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

func run(in, out string, jobs []job) []error {
	var mu sync.Mutex
	var errs []error

	ch := make(chan job)
	var wg sync.WaitGroup
	bar := pb.StartNew(len(jobs))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				if err := j.apply(in, out); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
				bar.Increment()
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	bar.Finish()
	return errs
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
	inside, err := isInside(in, out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if inside {
		fmt.Fprintf(os.Stderr, "output %s must be outside input %s\n", out, in)
		os.Exit(1)
	}

	items, walkErr := walk(in)
	if items == nil && walkErr != nil {
		fmt.Fprintf(os.Stderr, "walk %s: %v\n", in, walkErr)
		os.Exit(1)
	}

	errs := run(in, out, items)
	if walkErr != nil {
		errs = append(errs, walkErr)
	}
	if len(errs) > 0 {
		for _, err := range errs {
			fmt.Fprintln(os.Stderr, err)
		}
		fmt.Fprintf(os.Stderr, "%d of %d files failed\n", len(errs), len(items))
		os.Exit(1)
	}
}

// isInside reports whether out is in, equal to, or below in.
func isInside(in, out string) (bool, error) {
	absIn, err := filepath.Abs(in)
	if err != nil {
		return false, err
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(absIn, absOut)
	if err != nil {
		return false, nil
	}
	parent := rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
	return !parent, nil
}
