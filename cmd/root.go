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
	"syscall"
	"time"

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
// --debug and --trace are independent: stderr level reflects --debug (or
// cfg.debug), and --trace additionally writes a TRACE-level sink to disk.
func setupLogger(debugEnabled bool) func() {
	path := ""
	if flagTrace {
		path = tracePath
	}
	return initLogger(path, debugEnabled)
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

	// Per CDS:1 spec, SystemUpdateID should be a value that a returning
	// control point cannot mistake for "nothing changed since I last saw
	// you." We have no persistent state for it, so seed from the wall clock
	// (unix seconds): always increases across reboots, looks random to any
	// client that cached a value from before. Bumps from BumpUpdateID
	// monotonically increase from there during the run.
	uuid := newUUID()
	updateID := time.Now().Unix()
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

	// regenIdentity swaps the device UUID in-place: byebye-old, generate
	// new, alive-new. Used by both the UI button (OnRegenUUID) and the
	// automatic library-change trigger (OnAutoRegen).
	regenIdentity := func(reason string) {
		newUUID := newUUID()
		newLocation := fmt.Sprintf("http://%s:%d/device.xml", ip, cfg.Port)
		if ssdpSrv != nil {
			ssdpSrv.UpdateIdentity(newUUID, newLocation)
		}
		srv.UpdateIdentity(newUUID, cfg.Name)
		slog.Info("uuid regenerated",
			slog.String("reason", reason),
			slog.String("uuid", newUUID))
	}

	srv = server.New(server.Config{
		Port:    cfg.Port,
		Name:    cfg.Name,
		UUID:    uuid,
		IP:      ip,
		Library: lib,
		History: history,
		OnFileDelete: func() {
			// Reload so the web UI immediately reflects the deletion.
			// Do NOT bump SystemUpdateID here — the fsnotify watcher will
			// fire within ~2s and emit a single coalesced bump+NOTIFY for
			// TV subscribers. Bumping here too would double every event.
			if err := lib.Reload(cfg.MediaDir, cfg.RecentDays); err != nil {
				slog.Warn("rescan failed", slog.Any("err", err))
			}
		},
		OnRestartService: func() {
			if err := exec.Command("systemctl", "--user", "restart", "streambox").Run(); err != nil {
				slog.Warn("restart service failed", slog.Any("err", err))
			}
		},
		OnRegenUUID: func() { regenIdentity("ui") },
		OnAutoRegen: func() { regenIdentity("auto") },
		SendByebye: func(u string) {
			if ssdpSrv != nil {
				ssdpSrv.SendByebyeFor(u)
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
		srv.BumpUpdateID()
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
			srv.BumpUpdateID()
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
		slog.Info("streambox ready",
			slog.String("url", fmt.Sprintf("http://%s:%d", ip, cfg.Port)),
			slog.String("name", cfg.Name))
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

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
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
