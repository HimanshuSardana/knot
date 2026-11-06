package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSHSpec describes how to reach a device over SSH.
type SSHSpec struct {
	Name string
	Addr string
	Port int
	User string
	// Home is the directory to open first. If empty it is discovered with
	// SFTP's Getwd after connecting.
	Home string
	// Password is called only when key and agent auth are rejected, and only
	// when non-nil. It lets the phone work without keys.
	Password func() (string, error)
}

// SSH is a Node backed by an SSH/SFTP connection to another device.
//
// The connection is established lazily and re-established on demand, so
// building a Node never blocks and an offline device only fails when touched.
type SSH struct {
	spec SSHSpec

	mu     sync.Mutex
	client *ssh.Client
	sftp   *sftp.Client
	home   string
	closed bool

	// needsAuth records that the server rejected our credentials, so the UI can
	// offer a password prompt instead of showing a generic failure.
	needsAuth atomic.Bool
}

// NewSSH returns a Node for a remote device. No network activity happens until
// the first call.
func NewSSH(spec SSHSpec) *SSH {
	if spec.Port == 0 {
		spec.Port = 22
	}
	return &SSH{spec: spec, home: spec.Home}
}

func (s *SSH) Name() string { return s.spec.Name }
func (s *SSH) Kind() string { return "ssh" }

func (s *SSH) Home() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.home
}

func (s *SSH) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.resetLocked()
}

func (s *SSH) resetLocked() error {
	var err error
	if s.sftp != nil {
		err = s.sftp.Close()
		s.sftp = nil
	}
	if s.client != nil {
		if cerr := s.client.Close(); err == nil {
			err = cerr
		}
		s.client = nil
	}
	return err
}

// conn returns a live SFTP client, dialing if necessary. A stale connection is
// retried once, because tailnet peers drop out and come back.
func (s *SSH) conn(ctx context.Context) (*sftp.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, errors.New("node " + s.spec.Name + " is closed")
	}
	if s.sftp != nil {
		return s.sftp, nil
	}

	client, sftpClient, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	s.client, s.sftp = client, sftpClient

	if s.home == "" {
		if wd, err := sftpClient.Getwd(); err == nil {
			s.home = wd
		} else {
			s.home = "/"
		}
	}
	return s.sftp, nil
}

// withRetry runs fn against a connection, reconnecting once on failure.
func (s *SSH) withRetry(ctx context.Context, fn func(*sftp.Client) error) error {
	c, err := s.conn(ctx)
	if err != nil {
		return err
	}
	err = fn(c)
	if err == nil {
		return nil
	}
	if !isConnError(err) {
		return err
	}

	s.mu.Lock()
	_ = s.resetLocked()
	s.mu.Unlock()

	c, derr := s.conn(ctx)
	if derr != nil {
		return fmt.Errorf("%s: reconnect failed: %w (original: %v)", s.spec.Name, derr, err)
	}
	return fn(c)
}

// isConnError reports whether err looks like the transport died rather than the
// operation being invalid.
func isConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var status *sftp.StatusError
	if errors.As(err, &status) {
		// 7 = permission denied, 2 = no such file: real answers, not transport
		// problems. Everything else is treated as suspicious enough to retry.
		return status.Code != 2 && status.Code != 7
	}
	return false
}

func (s *SSH) dial(ctx context.Context) (*ssh.Client, *sftp.Client, error) {
	addr := net.JoinHostPort(s.spec.Addr, fmt.Sprint(s.spec.Port))

	auths, err := s.authMethods()
	if err != nil {
		return nil, nil, err
	}

	cfg := &ssh.ClientConfig{
		User:            s.spec.User,
		Auth:            auths,
		HostKeyCallback: hostKeyCallback(),
		Timeout:         15 * time.Second,
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	// Tie the handshake to ctx without leaking the connection.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		if isAuthError(err) {
			s.needsAuth.Store(true)
		}
		return nil, nil, fmt.Errorf("ssh %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Time{})

	client := ssh.NewClient(cc, chans, reqs)
	s.needsAuth.Store(false)
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("sftp %s: %w", addr, err)
	}
	return client, sftpClient, nil
}

// NeedsAuth reports whether the last connection attempt was rejected for lack
// of valid credentials, as opposed to the host being down.
func (s *SSH) NeedsAuth() bool { return s.needsAuth.Load() }

// CheckPassword verifies a password against the server using password auth
// alone, ignoring keys. Without this a working key would make any password look
// correct, and a bad one would be cached for later use.
func (s *SSH) CheckPassword(ctx context.Context, password string) error {
	addr := net.JoinHostPort(s.spec.Addr, fmt.Sprint(s.spec.Port))

	cfg := &ssh.ClientConfig{
		User: s.spec.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = password
				}
				return answers, nil
			}),
		},
		HostKeyCallback: hostKeyCallback(),
		Timeout:         20 * time.Second,
	}

	dialer := &net.Dialer{Timeout: 20 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		return fmt.Errorf("password rejected by %s: %w", s.spec.Name, err)
	}
	_ = ssh.NewClient(cc, chans, reqs).Close()
	return nil
}

// authMethods builds the auth chain: ssh-agent first, then the usual key files,
// then password/keyboard-interactive if a prompt is available.
func (s *SSH) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
	}

	if signers := keyFileSigners(); len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}

	if s.spec.Password != nil {
		// Termux's sshd offers keyboard-interactive; OpenSSH offers "password".
		// Registering both keeps one config working everywhere.
		pw := func() (string, error) { return s.spec.Password() }
		methods = append(methods, ssh.PasswordCallback(pw))
		methods = append(methods, ssh.KeyboardInteractive(
			func(name, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					pass, err := pw()
					if err != nil {
						return nil, err
					}
					answers[i] = pass
				}
				return answers, nil
			}))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no usable auth for %s: no ssh-agent, no key in ~/.ssh, no password prompt", s.spec.Name)
	}
	return methods, nil
}

// isAuthError distinguishes "wrong credentials" from "host unreachable".
func isAuthError(err error) bool {
	var authErr *ssh.ServerAuthError
	if errors.As(err, &authErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(msg, "no supported methods")
}

func keyFileSigners() []ssh.Signer {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	names := []string{"id_ed25519", "id_ecdsa", "id_rsa"}
	var signers []ssh.Signer
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(home, ".ssh", name))
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			// Passphrase-protected keys land here; the agent covers them.
			continue
		}
		signers = append(signers, signer)
	}
	return signers
}

var (
	hostKeyOnce sync.Once
	hostKeyFn   ssh.HostKeyCallback
)

// hostKeyCallback verifies against ~/.ssh/known_hosts when that file exists.
// A personal tailnet tool should not silently trust anything, but a missing
// known_hosts should not make the tool unusable either.
func hostKeyCallback() ssh.HostKeyCallback {
	hostKeyOnce.Do(func() {
		home, err := os.UserHomeDir()
		if err != nil {
			hostKeyFn = ssh.InsecureIgnoreHostKey()
			return
		}
		cb, err := knownhosts.New(filepath.Join(home, ".ssh", "known_hosts"))
		if err != nil {
			hostKeyFn = ssh.InsecureIgnoreHostKey()
			return
		}
		hostKeyFn = cb
	})
	return hostKeyFn
}

func (s *SSH) List(ctx context.Context, dir string) ([]Entry, error) {
	var entries []Entry
	err := s.withRetry(ctx, func(c *sftp.Client) error {
		infos, err := c.ReadDir(dir)
		if err != nil {
			return err
		}
		entries = make([]Entry, 0, len(infos))
		for _, info := range infos {
			entries = append(entries, entryFromInfo(info.Name(), info))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	SortEntries(entries)
	return entries, nil
}

func (s *SSH) Stat(ctx context.Context, p string) (Entry, error) {
	var entry Entry
	err := s.withRetry(ctx, func(c *sftp.Client) error {
		info, err := c.Stat(p)
		if err != nil {
			return err
		}
		entry = entryFromInfo(Base(p), info)
		return nil
	})
	return entry, err
}

func (s *SSH) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	var f *sftp.File
	err := s.withRetry(ctx, func(c *sftp.Client) error {
		var err error
		f, err = c.Open(p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s *SSH) Create(ctx context.Context, p string) (io.WriteCloser, error) {
	var f *sftp.File
	err := s.withRetry(ctx, func(c *sftp.Client) error {
		if dir := path.Dir(p); dir != "." && dir != "/" {
			if err := c.MkdirAll(dir); err != nil {
				return err
			}
		}
		var err error
		f, err = c.Create(p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s *SSH) Mkdir(ctx context.Context, p string) error {
	return s.withRetry(ctx, func(c *sftp.Client) error { return c.Mkdir(p) })
}

func (s *SSH) MkdirAll(ctx context.Context, p string) error {
	return s.withRetry(ctx, func(c *sftp.Client) error { return c.MkdirAll(p) })
}

func (s *SSH) Remove(ctx context.Context, p string) error {
	if p == "/" {
		return errors.New("refusing to remove /")
	}
	return s.withRetry(ctx, func(c *sftp.Client) error { return c.RemoveAll(p) })
}

func (s *SSH) Rename(ctx context.Context, from, to string) error {
	return s.withRetry(ctx, func(c *sftp.Client) error {
		// Some servers refuse to rename onto an existing target; remove first
		// so that "move" behaves the same locally and remotely.
		if _, err := c.Stat(to); err == nil {
			if err := c.RemoveAll(to); err != nil {
				return err
			}
		}
		return c.Rename(from, to)
	})
}
