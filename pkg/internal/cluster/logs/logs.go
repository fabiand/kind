/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package logs

import (
	"archive/tar"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/errors"
	"sigs.k8s.io/kind/pkg/exec"
	"sigs.k8s.io/kind/pkg/globals"
)

// Collect collects logs related to / from the cluster nodes and the host
// system to the specified directory
func Collect(nodes []nodes.Node, dir string) error {
	prefixedPath := func(path string) string {
		return filepath.Join(dir, path)
	}
	// helper to run a cmd and write the output to path
	execToPath := func(cmd exec.Cmd, path string) error {
		realPath := prefixedPath(path)
		if err := os.MkdirAll(filepath.Dir(realPath), os.ModePerm); err != nil {
			return err
		}
		f, err := os.Create(realPath)
		if err != nil {
			return err
		}
		defer f.Close()
		cmd.SetStdout(f)
		cmd.SetStderr(f)
		return cmd.Run()
	}
	execToPathFn := func(cmd exec.Cmd, path string) func() error {
		return func() error {
			return execToPath(cmd, path)
		}
	}
	// construct a slice of methods to collect logs
	fns := []func() error{
		// TODO(bentheelder): record the kind version here as well
		// record info about the host docker
		execToPathFn(
			exec.Command("docker", "info"),
			"docker-info.txt",
		),
	}

	// collect /var/log for each node and plan collecting more logs
	errs := []error{}
	for _, n := range nodes {
		node := n // https://golang.org/doc/faq#closures_and_goroutines
		name := node.String()
		if err := dumpDir(n, "/var/log", filepath.Join(dir, name)); err != nil {
			errs = append(errs, err)
		}

		fns = append(fns, func() error {
			return errors.AggregateConcurrent(
				// record info about the node container
				execToPathFn(
					exec.Command("docker", "inspect", name),
					filepath.Join(name, "inspect.json"),
				),
				// grab all of the node logs
				execToPathFn(
					exec.Command("docker", "logs", name),
					filepath.Join(name, "serial.log"),
				),
				execToPathFn(
					node.Command("cat", "/kind/version"),
					filepath.Join(name, "kubernetes-version.txt"),
				),
				execToPathFn(
					node.Command("journalctl", "--no-pager"),
					filepath.Join(name, "journal.log"),
				),
				execToPathFn(
					node.Command("journalctl", "--no-pager", "-u", "kubelet.service"),
					filepath.Join(name, "kubelet.log"),
				),
				execToPathFn(
					node.Command("journalctl", "--no-pager", "-u", "containerd.service"),
					filepath.Join(name, "containerd.log"),
				),
			)
		})
	}

	// run and collect up all errors
	errs = append(errs, errors.AggregateConcurrent(fns...))
	return errors.NewAggregate(errs)
}

// dumpDir dumps the dir nodeDir on the node to the dir hostDir on the host
func dumpDir(node nodes.Node, nodeDir, hostDir string) (err error) {
	// make tempdir to rsync nodeDir into (rsync handles taking a snapshot better)
	tmp, err := mktemp(node)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := node.Command("rm", "-rf", tmp).Run(); rerr != nil && err == nil {
			err = rerr
		}
	}()

	// rsync into the temp dir
	if err := node.Command("rsync", "--archive", path.Clean(nodeDir)+"/", tmp).Run(); err != nil {
		return err
	}

	// tar out to the host
	cmd := node.Command("tar", "--hard-dereference", "-C", tmp, "-chf", "-", ".")
	return exec.RunWithStdoutReader(cmd, func(outReader io.Reader) error {
		if err := untar(outReader, hostDir); err != nil {
			return errors.Wrapf(err, "Untarring %q: %v", nodeDir, err)
		}
		return nil
	})
}

// mktemp creates a tempdir on the node
func mktemp(node nodes.Node) (string, error) {
	lines, err := exec.OutputLines(node.Command("mktemp", "-d"))
	if err != nil {
		return "", err
	}
	if len(lines) != 1 {
		return "", errors.Errorf("invalid output from mktemp -d: %q", strings.Join(lines, "\n"))
	}
	return lines[0], nil
}

// untar reads the tar file from r and writes it into dir.
func untar(r io.Reader, dir string) (err error) {
	tr := tar.NewReader(r)
	for {
		f, err := tr.Next()

		switch {
		case err == io.EOF:
			return nil
		case err != nil:
			return errors.Wrapf(err, "tar reading error: %v", err)
		case f == nil:
			continue
		}

		rel := filepath.FromSlash(f.Name)
		abs := filepath.Join(dir, rel)

		switch f.Typeflag {
		case tar.TypeReg:
			wf, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, os.FileMode(f.Mode))
			if err != nil {
				return err
			}
			n, err := io.Copy(wf, tr)
			if closeErr := wf.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
			if err != nil {
				return errors.Errorf("error writing to %s: %v", abs, err)
			}
			if n != f.Size {
				return errors.Errorf("only wrote %d bytes to %s; expected %d", n, abs, f.Size)
			}
		case tar.TypeDir:
			if _, err := os.Stat(abs); err != nil {
				if err := os.MkdirAll(abs, 0755); err != nil {
					return err
				}
			}
		default:
			globals.GetLogger().Warnf("tar file entry %s contained unsupported file type %v", f.Name, f.Typeflag)
		}
	}
}
