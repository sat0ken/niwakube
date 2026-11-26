// niwakube-iso は installer/config.yaml から、ホストを自動インストールする ISO を作る。
//
//	niwakube-iso check  -config installer/config.yaml
//	niwakube-iso get    -config installer/config.yaml KEY       # 値を 1 つ表示する (スクリプト用)
//	niwakube-iso render -config installer/config.yaml            # user-data と grub.cfg を表示する
//	niwakube-iso build  -config installer/config.yaml -mini mini.iso -server server.env -out niwakube.iso
//
// server.env は installer/fetch-ubuntu.sh が作る (SERVER_ISO_VERSION / URL / SIZE / SHA256)。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sat0ken/niwakube/internal/installer"
	"github.com/sat0ken/niwakube/internal/sys"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "niwakube-iso:", err)
		os.Exit(1)
	}
}

func usage() error {
	return errors.New("usage: niwakube-iso check|get|render|build -config installer/config.yaml [...]")
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	config := fs.String("config", "installer/config.yaml", "設定ファイル")
	mini := fs.String("mini", "", "Ubuntu mini ISO (build)")
	server := fs.String("server", "", "Server ISO の情報 (fetch-ubuntu.sh が作る server.env)")
	out := fs.String("out", "niwakube-installer.iso", "出力する ISO (build)")
	var extraKeys multiFlag
	fs.Var(&extraKeys, "extra-ssh-key", "追加する SSH 公開鍵 (テスト用。複数指定可)")
	testMode := fs.Bool("test", false, "CI の VM でテストするための ISO にする (DHCP、一番大きいディスク、無人インストール、シリアルコンソール)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	c, err := installer.Load(*config)
	if err != nil {
		return err
	}
	c.User.SSHAuthorizedKeys = append(c.User.SSHAuthorizedKeys, extraKeys...)
	if *testMode {
		c.ForTest()
	}

	switch cmd {
	case "check":
		fmt.Print(c.Summary())
		return nil
	case "get":
		if fs.NArg() != 1 {
			return errors.New("get needs one KEY (hostname, user.name, ubuntu.release, niwakube.setup)")
		}
		switch fs.Arg(0) {
		case "hostname":
			fmt.Println(c.Hostname)
		case "user.name":
			fmt.Println(c.User.Name)
		case "ubuntu.release":
			fmt.Println(c.Ubuntu.Release)
		case "niwakube.setup":
			fmt.Println(c.Niwakube.Enabled())
		default:
			return fmt.Errorf("unknown key %q", fs.Arg(0))
		}
		return nil
	case "render":
		ud, err := c.UserData()
		if err != nil {
			return err
		}
		fmt.Printf("# ----- user-data\n%s", ud)
		if *server != "" {
			iso, err := readServerEnv(*server)
			if err != nil {
				return err
			}
			if c.Install.ISOMirror != "" {
				// render はネットワークに触らないので、ミラーにあるかは確かめない
				if u, err := installer.MirrorURL(c.Install.ISOMirror, iso.URL); err == nil {
					iso.Mirrors = []string{u}
				}
			}
			g, err := c.GrubConfig(iso)
			if err != nil {
				return err
			}
			fmt.Printf("# ----- grub.cfg\n%s", g)
		}
		return nil
	case "build":
		if *mini == "" || *server == "" {
			return errors.New("build needs -mini and -server")
		}
		iso, err := readServerEnv(*server)
		if err != nil {
			return err
		}
		fmt.Print(c.Summary())
		if gh := c.User.GitHubKeys; gh != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			n, err := c.EmbedGitHubKeys(ctx, http.DefaultClient)
			cancel()
			if err != nil {
				return fmt.Errorf("user.github_keys: %w", err)
			}
			fmt.Printf("ssh keys: embedded %d key(s) from github.com/%s.keys\n", n, gh)
		}
		iso = withMirror(c, iso)
		if err := c.Build(context.Background(), sys.ExecRunner{}, installer.BuildOptions{MiniISO: *mini, Server: iso, Output: *out}); err != nil {
			return err
		}
		fmt.Printf("wrote %s (downloads Ubuntu Server %s at install time)\n", *out, iso.Version)
		return nil
	default:
		return usage()
	}
}

// withMirror は install.iso_mirror に同じ ISO があれば、先に試す URL として足す。
// 無ければ (同期が遅れているなど) 警告を出して releases.ubuntu.com だけにする。
func withMirror(c *installer.Config, iso installer.ServerISO) installer.ServerISO {
	if c.Install.ISOMirror == "" {
		return iso
	}
	u, err := installer.MirrorURL(c.Install.ISOMirror, iso.URL)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err = installer.CheckMirror(ctx, http.DefaultClient, u, iso.Size)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: install.iso_mirror is not used: %v\n", err)
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			fmt.Printf("::warning title=install.iso_mirror::not used (downloads from releases.ubuntu.com): %v\n", err)
		}
		return iso
	}
	fmt.Printf("iso mirror: %s\n", u)
	iso.Mirrors = []string{u}
	return iso
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func readServerEnv(path string) (installer.ServerISO, error) {
	f, err := os.Open(path)
	if err != nil {
		return installer.ServerISO{}, err
	}
	defer f.Close()
	var s installer.ServerISO
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch k {
		case "SERVER_ISO_VERSION":
			s.Version = v
		case "SERVER_ISO_URL":
			s.URL = v
		case "SERVER_ISO_SIZE":
			if s.Size, err = strconv.ParseInt(v, 10, 64); err != nil {
				return s, fmt.Errorf("SERVER_ISO_SIZE: %w", err)
			}
		case "SERVER_ISO_SHA256":
			s.SHA256 = v
		}
	}
	return s, sc.Err()
}
