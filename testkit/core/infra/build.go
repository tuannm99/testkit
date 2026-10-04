package infra

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuannm99/testkit/testkit/core/config"
)

// buildEnvArgs forwards proxy settings and an optional extra CA bundle to
// `docker build`, so images also build behind a TLS-intercepting proxy.
//   - HTTP(S)_PROXY/NO_PROXY from the environment become build args; when the
//     proxy listens on localhost the build uses the host network.
//   - TESTKIT_BUILD_CA=<pem file> is mounted as the BuildKit secret extra_ca;
//     Dockerfiles append it to the trust store only for the build steps.
func buildEnvArgs() []string {
	var args []string
	hostNet := false
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if v := os.Getenv(k); v != "" {
			args = append(args, "--build-arg", k+"="+v)
			if strings.Contains(v, "localhost") || strings.Contains(v, "127.0.0.1") {
				hostNet = true
			}
		}
	}
	if hostNet {
		args = append(args, "--network", "host")
	}
	if ca := os.Getenv("TESTKIT_BUILD_CA"); ca != "" {
		args = append(args, "--secret", "id=extra_ca,src="+ca)
	}
	return args
}

// pinnedBuildArgs passes the pinned base images to every Dockerfile, so that
// builder/base images are pinned in versions.env too.
func pinnedBuildArgs(p *config.Project) []string {
	var args []string
	for _, k := range []string{"GO_BUILD_IMAGE", "BASE_IMAGE", "DOCKER_CLI_IMAGE", "OTEL_COLLECTOR_IMAGE", "TOXIPROXY_VERSION"} {
		if v := p.Get(k); v != "" {
			args = append(args, "--build-arg", k+"="+v)
		}
	}
	return args
}

func (s *Stack) imageExists(ctx context.Context, ref string) bool {
	_, err := s.D.Run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	return err == nil
}

// LabelSourceHash records the hash of the build context an image was built from.
const LabelSourceHash = "testkit.source_hash"

// upToDate reports whether ref exists and was built from the current sources.
func (s *Stack) upToDate(ctx context.Context, ref, hash string) bool {
	out, err := s.D.Run(ctx, "image", "inspect", "--format", `{{index .Config.Labels "`+LabelSourceHash+`"}}`, ref)
	return err == nil && strings.TrimSpace(out) == hash
}

// SourceHash hashes every file of a build context (tests, VCS and output
// directories excluded) so images are rebuilt exactly when their sources change.
func SourceHash(dir string, extra ...string) (string, error) {
	h := sha256.New()
	for _, e := range extra {
		io.WriteString(h, e+"\n")
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case ".git", "out", "bin", "node_modules", "test-results", "playwright-report":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		io.WriteString(h, rel+"\x00")
		_, err = io.Copy(h, f)
		return err
	})
	return hex.EncodeToString(h.Sum(nil))[:16], err
}

// EnsureImage builds one of the images declared under images: in testkit.yaml
// when it is missing (or always with force).
func (s *Stack) EnsureImage(ctx context.Context, name string, force bool) error {
	ib, ok := s.P.Images[name]
	if !ok {
		return fmt.Errorf("testkit.yaml: no image %q declared", name)
	}
	ref := s.P.ImageTag(name)
	hash, err := SourceHash(s.P.Abs(ib.Context), ib.Target, ib.Dockerfile)
	if err != nil {
		return err
	}
	if !force && s.upToDate(ctx, ref, hash) {
		return nil
	}
	fmt.Fprintf(s.Out, "building %s (sources %s)\n", ref, hash)
	args := []string{"build", "-t", ref, "-f", s.P.Abs(ib.Dockerfile),
		"--label", LabelManaged + "=true", "--label", LabelSourceHash + "=" + hash,
		"--build-arg", "TESTKIT_VERSION=" + s.P.Get("TESTKIT_VERSION")}
	if ib.Target != "" {
		args = append(args, "--target", ib.Target)
	}
	for k, v := range ib.Args {
		args = append(args, "--build-arg", k+"="+v)
	}
	args = append(args, pinnedBuildArgs(s.P)...)
	args = append(args, buildEnvArgs()...)
	args = append(args, s.P.Abs(ib.Context))
	return s.D.Stream(ctx, s.Out, args...)
}

// EnsureServiceImages builds the images of a service under test when they
// are missing: the release image and, when declared, the test image (built
// with the failpoint build tag; only that one is used for mutation/crash tests).
func (s *Stack) EnsureServiceImages(ctx context.Context, svc *config.Service, force bool) error {
	b := svc.Image.Build
	if b == nil {
		// Pulled image: nothing to build, docker pulls it on first use.
		return nil
	}
	build := func(ref string, extra map[string]string) error {
		hash, err := SourceHash(svc.Path(b.Context), fmt.Sprint(extra))
		if err != nil {
			return err
		}
		if !force && s.upToDate(ctx, ref, hash) {
			return nil
		}
		fmt.Fprintf(s.Out, "building %s (sources %s)\n", ref, hash)
		dockerfile := "Dockerfile"
		if b.Dockerfile != "" {
			dockerfile = b.Dockerfile
		}
		args := []string{"build", "-t", ref, "-f", svc.Path(b.Context + "/" + dockerfile),
			"--label", LabelManaged + "=true", "--label", LabelService + "=" + svc.Name, "--label", LabelSourceHash + "=" + hash}
		for k, v := range b.Args {
			args = append(args, "--build-arg", k+"="+v)
		}
		for k, v := range extra {
			args = append(args, "--build-arg", k+"="+v)
		}
		args = append(args, pinnedBuildArgs(s.P)...)
		args = append(args, buildEnvArgs()...)
		args = append(args, svc.Path(b.Context))
		return s.D.Stream(ctx, s.Out, args...)
	}
	if err := build(svc.ImageRef(false), nil); err != nil {
		return err
	}
	if svc.Image.TestTag != "" {
		return build(svc.ImageRef(true), b.TestArgs)
	}
	return nil
}
