package cmd

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"streambox/internal/config"
	"streambox/internal/media"
	"streambox/internal/server"
	"streambox/internal/ssdp"

	"github.com/spf13/cobra"
)

// Version is set at build time via ldflags.
var Version = "dev"

// tracePath is where the --trace flag writes (truncated on every start).
const tracePath = "/tmp/streambox.log"

var (
	flagDebug bool
	flagTrace bool
)

var rootCmd = &cobra.Command{
	Use:   "streambox",
	Short: "DLNA media server for video files",
	RunE:  runServe,
}

// Execute runs the CLI.
func Execute() {
	rootCmd.Version = Version
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().BoolVarP(&flagDebug, "debug", "d", false,
		"Debug-level logging on stderr.")
	rootCmd.PersistentFlags().BoolVar(&flagTrace, "trace", false,
		"Trace-level logs to "+tracePath+" (truncated each run).")

	// Pre-register --version (no short alias) so cobra's default doesn't add -v.
	rootCmd.Flags().Bool("version", false, "Print the version and exit.")

	rootCmd.Flags().StringP("config", "c", "", "Path to TOML config file")
	rootCmd.Flags().StringP("media", "m", "", "Directory to serve video files from")
	rootCmd.Flags().IntP("port", "p", 0, "HTTP port (default 8080)")
	rootCmd.Flags().StringP("name", "n", "", "Friendly device name shown on the TV (default \"StreamBox\")")
	rootCmd.Flags().StringP("iface", "i", "", "Network interface for SSDP (default: auto-detect)")
}

// setupLogger configures slog from the persistent --debug / --trace flags,
// plus the resolved config.debug field. Returns a cleanup func to defer.
//
// Precedence (highest first): --trace, --debug, cfg.debug, default.
func setupLogger(debugEnabled bool) func() {
	level, path := "", ""
	switch {
	case flagTrace:
		level, path = "trace", tracePath
	case debugEnabled:
		level = "debug"
	}
	return initLogger(path, level)
}

func runServe(cmd *cobra.Command, args []string) error {
	cfg := config.Defaults()

	cfgFile, err := cmd.Flags().GetString("config")
	if err != nil {
		slog.Log(cmd.Context(), LevelTrace, "get flag config", "err", err)
	}
	if cfgFile == "" {
		if cfgDir, err := os.UserConfigDir(); err == nil {
			def := filepath.Join(cfgDir, "streambox", "config.toml")
			if _, err := os.Stat(def); err == nil {
				cfgFile = def
			}
		}
	}
	if cfgFile != "" {
		loaded, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("loading config file %q: %w", cfgFile, err)
		}
		cfg = loaded
	}

	if cmd.Flags().Changed("media") {
		v, ferr := cmd.Flags().GetString("media")
		if ferr != nil {
			slog.Log(cmd.Context(), LevelTrace, "get flag media", "err", ferr)
		}
		cfg.MediaDir = v
	}
	if cmd.Flags().Changed("port") {
		v, ferr := cmd.Flags().GetInt("port")
		if ferr != nil {
			slog.Log(cmd.Context(), LevelTrace, "get flag port", "err", ferr)
		}
		cfg.Port = v
	}
	if cmd.Flags().Changed("name") {
		v, ferr := cmd.Flags().GetString("name")
		if ferr != nil {
			slog.Log(cmd.Context(), LevelTrace, "get flag name", "err", ferr)
		}
		cfg.Name = v
	}
	cfg.MediaDir = expandHome(cfg.MediaDir)

	cleanup := setupLogger(flagDebug || cfg.Debug)
	defer cleanup()

	lib, err := media.NewLibrary(cfg.MediaDir, cfg.RecentDays, cfg.RecentBuckets)
	if err != nil {
		return fmt.Errorf("scanning media directory %q: %w", cfg.MediaDir, err)
	}
	slog.Info("library scanned",
		slog.Int("videos", lib.VideoCount()),
		slog.String("dir", cfg.MediaDir),
		slog.Int("recent_days", cfg.RecentDays))

	ifaceName, ferr := cmd.Flags().GetString("iface")
	if ferr != nil {
		slog.Log(cmd.Context(), LevelTrace, "get flag iface", "err", ferr)
	}
	ip, err := detectIP(ifaceName)
	if err != nil {
		return fmt.Errorf("detecting local IP: %w", err)
	}
	slog.Info("advertising", slog.String("url", fmt.Sprintf("http://%s:%d", ip, cfg.Port)))

	uuid, err := loadOrCreateUUID()
	if err != nil {
		return fmt.Errorf("loading uuid: %w", err)
	}
	if gen := loadGeneration(); gen > 1 {
		cfg.Name = fmt.Sprintf("%s %d", cfg.Name, gen)
	}
	updateID := loadUpdateID() + 1 // bump on startup to invalidate stale TV caches
	if err := saveUpdateID(updateID); err != nil {
		return fmt.Errorf("saving updateid: %w", err)
	}
	location := fmt.Sprintf("http://%s:%d/device.xml", ip, cfg.Port)

	var iface *net.Interface
	if ifaceName != "" {
		iface, err = net.InterfaceByName(ifaceName)
		if err != nil {
			return fmt.Errorf("interface %q: %w", ifaceName, err)
		}
	}

	history := &media.WatchHistory{}

	var srv *server.Server
	var ssdpSrv *ssdp.Server
	srv = server.New(server.Config{
		Port:    cfg.Port,
		Name:    cfg.Name,
		UUID:    uuid,
		IP:      ip,
		Library: lib,
		History: history,
		OnFileDelete: func() {
			if err := lib.Reload(cfg.MediaDir, cfg.RecentDays); err != nil {
				slog.Warn("rescan failed", slog.Any("err", err))
				return
			}
			if err := saveUpdateID(srv.BumpUpdateID()); err != nil {
				slog.Warn("save updateid failed", slog.Any("err", err))
			}
		},
		OnRestartService: func() {
			if err := exec.Command("systemctl", "--user", "restart", "streambox").Run(); err != nil {
				slog.Warn("restart service failed", slog.Any("err", err))
			}
		},
		OnRegenUUID: func() {
			if err := saveGeneration(loadGeneration() + 1); err != nil {
				slog.Warn("regen uuid: save generation failed", slog.Any("err", err))
			}
			if path, err := uuidPath(); err == nil {
				if rerr := os.Remove(path); rerr != nil {
					slog.Log(context.Background(), LevelTrace, "regen uuid: remove file", "path", path, "err", rerr)
				}
			}
			if err := exec.Command("systemctl", "--user", "restart", "streambox").Run(); err != nil {
				slog.Warn("regen uuid restart failed", slog.Any("err", err))
			}
		},
	})
	srv.SetUpdateID(updateID)

	ssdpSrv = ssdp.New(uuid, location, iface)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := media.Watch(ctx, cfg.MediaDir, func() {
		slog.Info("media changed, rescanning", slog.String("dir", cfg.MediaDir))
		if err := lib.Reload(cfg.MediaDir, cfg.RecentDays); err != nil {
			slog.Warn("rescan failed", slog.Any("err", err))
			return
		}
		slog.Info("rescan complete", slog.Int("videos", lib.VideoCount()))
		if err := saveUpdateID(srv.BumpUpdateID()); err != nil {
			slog.Warn("save updateid failed", slog.Any("err", err))
		}
		ssdpSrv.SendAlive()
	}); err != nil {
		slog.Warn("media watcher unavailable", slog.Any("err", err))
	}

	if cfg.Flatten {
		if err := media.WatchAndFlatten(ctx, cfg.MediaDir, func() {
			slog.Info("flatten complete, rescanning", slog.String("dir", cfg.MediaDir))
			if err := lib.Reload(cfg.MediaDir, cfg.RecentDays); err != nil {
				slog.Warn("rescan failed", slog.Any("err", err))
				return
			}
			slog.Info("rescan complete", slog.Int("videos", lib.VideoCount()))
			if err := saveUpdateID(srv.BumpUpdateID()); err != nil {
				slog.Warn("save updateid failed", slog.Any("err", err))
			}
			ssdpSrv.SendAlive()
		}); err != nil {
			slog.Warn("flatten watcher unavailable", slog.Any("err", err))
		}
	}

	go func() {
		if err := ssdpSrv.Start(ctx); err != nil && ctx.Err() == nil {
			slog.Error("ssdp stopped", slog.Any("err", err))
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", slog.Int("port", cfg.Port))
		errCh <- srv.ListenAndServe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-sig:
		slog.Info("shutting down")
		return nil
	}
}

func detectIP(ifaceName string) (string, error) {
	if ifaceName != "" {
		iface, err := net.InterfaceByName(ifaceName)
		if err != nil {
			return "", fmt.Errorf("interface %q: %w", ifaceName, err)
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", fmt.Errorf("interface %q addrs: %w", ifaceName, err)
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() {
				return ipnet.IP.String(), nil
			}
		}
		return "", fmt.Errorf("no IPv4 address on interface %q", ifaceName)
	}
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", fmt.Errorf("dial outbound for ip detection: %w", err)
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil {
			slog.Log(context.Background(), LevelTrace, "ip detect: close conn", "err", cerr)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// dataDir returns the directory for persistent app state, honouring
// $XDG_DATA_HOME and falling back to ~/.local/share/streambox.
func dataDir() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "streambox"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "streambox"), nil
}

func uuidPath() (string, error) {
	d, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "uuid"), nil
}

func updateIDPath() (string, error) {
	d, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "updateid"), nil
}

func loadOrCreateUUID() (string, error) {
	path, err := uuidPath()
	if err != nil {
		return newUUID(), nil
	}
	if data, err := os.ReadFile(path); err == nil {
		if u := strings.TrimSpace(string(data)); u != "" {
			return u, nil
		}
	}
	u := newUUID()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("creating data dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(u+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("writing uuid: %w", err)
	}
	return u, nil
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func generationPath() (string, error) {
	d, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "generation"), nil
}

// loadGeneration returns the current name-generation counter (1 = first/default).
func loadGeneration() int {
	path, err := generationPath()
	if err != nil {
		return 1
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 1
	}
	n, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if perr != nil || n < 1 {
		return 1
	}
	return n
}

func saveGeneration(n int) error {
	path, err := generationPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(n)+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing generation: %w", err)
	}
	return nil
}

func loadUpdateID() int64 {
	path, err := updateIDPath()
	if err != nil {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	id, perr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if perr != nil {
		slog.Log(context.Background(), LevelTrace, "loadUpdateID: parse int", "err", perr)
	}
	return id
}

func saveUpdateID(id int64) error {
	path, err := updateIDPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(strconv.FormatInt(id, 10)+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing updateid: %w", err)
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" || p == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			slog.Log(context.Background(), LevelTrace, "expandHome: user home dir", "err", err)
		}
		return home
	}
	if len(p) >= 2 && p[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			slog.Log(context.Background(), LevelTrace, "expandHome: user home dir", "err", err)
		}
		return filepath.Join(home, p[2:])
	}
	return p
}
