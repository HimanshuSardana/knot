// Command knot browses and moves files across the devices on a tailnet.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/HimanshuSardana/knot/internal/cluster"
	"github.com/HimanshuSardana/knot/internal/config"
	"github.com/HimanshuSardana/knot/internal/node"
	"github.com/HimanshuSardana/knot/internal/ops"
	"github.com/HimanshuSardana/knot/internal/tui"
	"github.com/HimanshuSardana/knot/internal/web"
)

const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "knot:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]

	// A bare invocation is the TUI; anything else is a subcommand.
	if len(args) > 0 {
		switch args[0] {
		case "serve", "web":
			return cmdServe(args[1:])
		case "nodes", "devices":
			return cmdNodes()
		case "cp", "copy", "mv", "move":
			return cmdCopy(args[0], args[1:])
		case "version", "-v", "--version":
			fmt.Println("knot", version)
			return nil
		case "help", "-h", "--help":
			usage()
			return nil
		}
	}

	return cmdTUI()
}

func usage() {
	fmt.Print(`knot — browse and move files across your tailnet

usage:
  knot                     open the two-pane terminal browser
  knot serve [--listen A]  serve the web UI (default :8787)
  knot nodes               list configured devices and tailnet status
  knot cp <src> <dst>      copy between devices
  knot mv <src> <dst>      move between devices

device paths are written as name:/path, for example:
  knot cp archbox:/home/me/photo.jpg phone:/sdcard/DCIM/
  knot cp ./notes.md thinkpad:/home/me/notes/

config: ` + config.Path() + `
`)
}

func loadCluster() (*cluster.Cluster, *config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	cl, err := cluster.Open(cfg)
	if err != nil {
		return nil, nil, err
	}
	return cl, cfg, nil
}

// ---------------------------------------------------------------- TUI

func cmdTUI() error {
	cl, _, err := loadCluster()
	if err != nil {
		return err
	}
	defer cl.Close()

	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	if interactive {
		// The prompt is only usable before bubbletea takes the screen, so it is
		// installed for the preflight and removed afterwards.
		cl.Passwords.Prompt = terminalPrompt
		preflight(cl)
		cl.Passwords.Prompt = nil
	}

	p := tea.NewProgram(tui.New(cl), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// preflight touches every SSH node so credentials can be gathered up front and
// so unreachable devices are reported before the full-screen UI appears.
func preflight(cl *cluster.Cluster) {
	var wg sync.WaitGroup
	failed := make(chan string, len(cl.Names()))

	for _, n := range cl.Nodes() {
		if n.Kind() != "ssh" {
			continue
		}
		wg.Add(1)
		go func(n node.Node) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			home := n.Home()
			if home == "" {
				home = "/"
			}
			if _, err := n.List(ctx, home); err != nil {
				failed <- n.Name()
			}
		}(n)
	}

	wg.Wait()
	close(failed)

	for name := range failed {
		fmt.Fprintf(os.Stderr, "knot: %s is not reachable right now\n", name)
	}
}

// terminalPrompt asks for a password without echoing it.
func terminalPrompt(name string) (string, error) {
	fmt.Fprintf(os.Stderr, "password for %s: ", name)
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(pw), nil
}

// ---------------------------------------------------------------- serve

func cmdServe(args []string) error {
	cl, cfg, err := loadCluster()
	if err != nil {
		return err
	}
	defer cl.Close()

	listen := cfg.Web.Listen
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--listen", "-l":
			if i+1 < len(args) {
				listen = args[i+1]
				i++
			}
		case "--token", "-t":
			if i+1 < len(args) {
				cfg.Web.Token = args[i+1]
				i++
			}
		}
	}

	token := cfg.Web.Token
	if token == "" {
		token = os.Getenv("KNOT_TOKEN")
	}

	// A bare ":port" is interpreted as "tailnet only": binding to every
	// interface would expose all four filesystems to the LAN and beyond.
	if strings.HasPrefix(listen, ":") {
		if ip := cl.SelfIP(); ip != "" {
			listen = ip + listen
		} else {
			listen = "127.0.0.1" + listen
		}
	}

	srv := web.New(cl, token)

	// The web UI cannot prompt on a terminal, so missing passwords are reported
	// through the API instead of blocking a request.
	cl.Passwords.Prompt = nil

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("knot %s — web UI on http://%s\n", version, listen)
	fmt.Printf("open:  http://%s/?t=%s\n", displayAddr(listen), srv.Token())
	fmt.Printf("token: %s\n", srv.Token())
	return srv.Serve(ctx, listen)
}

// displayAddr turns :8787 into something clickable.
func displayAddr(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "localhost" + listen
	}
	return listen
}

// ---------------------------------------------------------------- nodes

func cmdNodes() error {
	cl, cfg, err := loadCluster()
	if err != nil {
		return err
	}
	defer cl.Close()

	fmt.Printf("%-12s %-7s %-16s %-8s %s\n", "NODE", "KIND", "ADDRESS", "STATUS", "HOME")
	for _, info := range cl.Info() {
		status := "offline"
		if info.Online {
			status = "online"
		}
		home := info.Home
		if home == "" {
			home = "(discovered on connect)"
		}
		addr := info.Addr
		if addr == "" {
			addr = "-"
		}
		fmt.Printf("%-12s %-7s %-16s %-8s %s\n", info.Name, info.Kind, addr, status, home)
	}
	fmt.Printf("\nconfig: %s\n", config.Path())
	_ = cfg
	return nil
}

// ---------------------------------------------------------------- cp/mv

func cmdCopy(verb string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: knot %s <src> <dst>", verb)
	}

	cl, _, err := loadCluster()
	if err != nil {
		return err
	}
	defer cl.Close()

	cl.Passwords.Prompt = terminalPrompt

	srcNode, srcPath, err := resolveSpec(cl, args[0])
	if err != nil {
		return err
	}
	dstNode, dstPath, err := resolveSpec(cl, args[1])
	if err != nil {
		return err
	}

	// "knot cp a.txt phone:/sdcard/" means "into that directory".
	if len(args[1]) > 0 && strings.HasSuffix(args[1], "/") && args[1] != "/" {
		dstPath = node.Join(dstPath, node.Base(srcPath))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lastPrint time.Time
	progress := func(copied, total int64) {
		if time.Since(lastPrint) < 100*time.Millisecond && (total == 0 || copied < total) {
			return
		}
		lastPrint = time.Now()
		if total > 0 {
			fmt.Fprintf(os.Stderr, "\r%s %d%% (%s / %s)", verb, copied*100/total, human(copied), human(total))
		} else {
			fmt.Fprintf(os.Stderr, "\r%s %s", verb, human(copied))
		}
	}

	if verb == "mv" || verb == "move" {
		err = ops.Move(ctx, srcNode, srcPath, dstNode, dstPath, progress)
	} else {
		err = ops.Copy(ctx, srcNode, srcPath, dstNode, dstPath, progress)
	}
	fmt.Fprintln(os.Stderr)
	return err
}

// resolveSpec parses "name:/path". A bare path is taken as local.
func resolveSpec(cl *cluster.Cluster, spec string) (node.Node, string, error) {
	idx := strings.Index(spec, ":")
	if idx < 0 {
		// No node prefix: use the local node.
		for _, n := range cl.Nodes() {
			if n.Kind() == "local" {
				abs, err := absPath(spec)
				if err != nil {
					return nil, "", err
				}
				return n, abs, nil
			}
		}
		return nil, "", fmt.Errorf("no local node configured for path %q", spec)
	}

	name := spec[:idx]
	path := spec[idx+1:]
	if path == "" {
		path = "/"
	}
	n, err := cl.Node(name)
	if err != nil {
		return nil, "", err
	}
	return n, path, nil
}

func absPath(p string) (string, error) {
	if strings.HasPrefix(p, "/") {
		return p, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return node.Join(wd, p), nil
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTP"[exp])
}
