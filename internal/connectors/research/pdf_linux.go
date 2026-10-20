//go:build linux

package research

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type cappedPDFOutput struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *cappedPDFOutput) Len() int       { return b.buffer.Len() }
func (b *cappedPDFOutput) Bytes() []byte  { return b.buffer.Bytes() }
func (b *cappedPDFOutput) String() string { return b.buffer.String() }

func (b *cappedPDFOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.exceeded = true
		return 0, errors.New("PDF output limit")
	}
	return b.buffer.Write(p)
}

func runPDFCommand(ctx context.Context, data []byte, tool string, args ...string) ([]byte, string) {
	if ctx.Err() != nil {
		return nil, "pdf_timeout"
	}
	// The sandbox is mandatory: no network, home, credentials, or source paths.
	// Missing dependencies or disabled user namespaces produce an evidence gap.
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, "pdf_extractor_unavailable"
	}
	parser, err := exec.LookPath(tool)
	if err != nil {
		return nil, "pdf_extractor_unavailable"
	}
	limiter, err := exec.LookPath("prlimit")
	if err != nil {
		return nil, "pdf_extractor_unavailable"
	}
	argv := []string{"--unshare-all", "--die-with-parent", "--cap-drop", "ALL", "--clearenv", "--setenv", "LC_ALL", "C", "--setenv", "HOME", "/tmp", "--tmpfs", "/tmp", "--proc", "/proc", "--dev", "/dev", "--chdir", "/tmp"}
	for _, dir := range []string{"/usr", "/lib", "/lib64", "/etc/fonts"} {
		if _, err := os.Stat(dir); err == nil {
			argv = append(argv, "--ro-bind", dir, dir)
		}
	}
	argv = append(argv, "--", limiter, "--as=536870912", "--cpu=3", "--fsize=0", "--nofile=64", "--core=0", "--", parser)
	argv = append(argv, args...)
	command := exec.CommandContext(ctx, bwrap, argv...)
	command.Env = []string{"LC_ALL=C"}
	command.Stdin = bytes.NewReader(data)
	out, diagnostic := &cappedPDFOutput{limit: maxPDFOutput}, &cappedPDFOutput{limit: 16 << 10}
	command.Stdout, command.Stderr = out, diagnostic
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = 250 * time.Millisecond
	err = command.Run()
	if ctx.Err() != nil {
		return nil, "pdf_timeout"
	}
	if out.exceeded || diagnostic.exceeded {
		return nil, "pdf_output_limit"
	}
	if strings.Contains(strings.ToLower(diagnostic.String()), "password") || strings.Contains(strings.ToLower(diagnostic.String()), "encrypted") {
		return nil, "pdf_encrypted"
	}
	if strings.Contains(diagnostic.String(), "bwrap:") || strings.Contains(diagnostic.String(), "prlimit:") {
		return nil, "pdf_sandbox_unavailable"
	}
	if err != nil || diagnostic.Len() != 0 {
		return nil, "pdf_malformed"
	}
	return out.Bytes(), ""
}

var _ io.Writer = (*cappedPDFOutput)(nil)
