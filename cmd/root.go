package cmd

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"net"
	"os"
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

// Version is set at build time via -ldflags.
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:   "streambox",
	Short: "DLNA media server for video files",
	RunE:  runServe,
}

// Execute runs the CLI.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.Flags().StringP("config", "c", "", "Path to TOML config file")
	rootCmd.Flags().StringP("media", "m", "", "Directory to serve video files from")
	rootCmd.Flags().IntP("port", "p", 0, "HTTP port (default 8080)")
	rootCmd.Flags().StringP("name", "n", "", "Friendly device name shown on the TV (default \"StreamBox\")")
	rootCmd.Flags().StringP("iface", "i", "", "Network interface for SSDP (default: auto-detect)")
	rootCmd.Flags().BoolP("debug", "d", false, "Enable debug logging")
}

func runServe(cmd *cobra.Command, args []string) error {
	// Load config: start from defaults, then overlay TOML file, then CLI flags.
	cfg := config.Defaults()

	cfgFile, _ := cmd.Flags().GetString("config")
	if cfgFile == "" {
		// Auto-detect default config location.
		if home, err := os.UserHomeDir(); err == nil {
			def := filepath.Join(home, ".config", "streambox", "config.toml")
			if _, err := os.Stat(def); err == nil {
				cfgFile = def
			}
		}
	}
	if cfgFile != "" {
		loaded, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("loading config file: %w", err)
		}
		cfg = loaded
	}

	// CLI flags override config file (only when explicitly provided).
	if cmd.Flags().Changed("media") {
		cfg.MediaDir, _ = cmd.Flags().GetString("media")
	}
	if cmd.Flags().Changed("port") {
		cfg.Port, _ = cmd.Flags().GetInt("port")
	}
	if cmd.Flags().Changed("name") {
		cfg.Name, _ = cmd.Flags().GetString("name")
	}
	if cmd.Flags().Changed("debug") {
		cfg.Debug, _ = cmd.Flags().GetBool("debug")
	}
	cfg.MediaDir = expandHome(cfg.MediaDir)

	// Configure logging.
	if cfg.Debug {
		log.SetFlags(log.LstdFlags | log.Lshortfile)
		if cfg.LogFile != "" {
			f, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				return fmt.Errorf("opening log file %q: %w", cfg.LogFile, err)
			}
			defer f.Close()
			log.SetOutput(io.MultiWriter(os.Stderr, f))
		}
		log.Println("debug mode enabled")
	}

	lib, err := media.NewLibrary(cfg.MediaDir, cfg.RecentDays)
	if err != nil {
		return fmt.Errorf("scanning media directory: %w", err)
	}
	log.Printf("Found %d video files in %s (recent cutoff: %d days)",
		lib.VideoCount(), cfg.MediaDir, cfg.RecentDays)

	ifaceName, _ := cmd.Flags().GetString("iface")
	ip, err := detectIP(ifaceName)
	if err != nil {
		return fmt.Errorf("detecting local IP: %w", err)
	}
	log.Printf("Advertising as http://%s:%d", ip, cfg.Port)

	uuid := loadOrCreateUUID()
	updateID := loadUpdateID() + 1 // always bump on startup to invalidate stale caches
	saveUpdateID(updateID)
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
		Debug:   cfg.Debug,
		Library: lib,
		History: history,
		OnFileDelete: func() {
			if err := lib.Reload(cfg.MediaDir, cfg.RecentDays); err != nil {
				log.Printf("Rescan error: %v", err)
				return
			}
			saveUpdateID(srv.BumpUpdateID())
		},
		OnRefresh: func() {
			if ssdpSrv != nil {
				ssdpSrv.SendAlive()
			}
		},
	})
	srv.SetUpdateID(updateID)

	ssdpSrv = ssdp.New(uuid, location, iface, cfg.Debug)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := media.Watch(ctx, cfg.MediaDir, func() {
		log.Printf("Media directory changed, rescanning %s", cfg.MediaDir)
		if err := lib.Reload(cfg.MediaDir, cfg.RecentDays); err != nil {
			log.Printf("Rescan error: %v", err)
			return
		}
		log.Printf("Rescan complete: %d video files", lib.VideoCount())
		saveUpdateID(srv.BumpUpdateID())
		ssdpSrv.SendAlive()
	}); err != nil {
		log.Printf("Media watcher unavailable: %v", err)
	}

	go func() {
		if err := ssdpSrv.Start(ctx); err != nil && ctx.Err() == nil {
			log.Printf("SSDP error: %v", err)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("Serving on :%d", cfg.Port)
		errCh <- srv.ListenAndServe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-sig:
		log.Println("Shutting down")
		return nil
	}
}

func detectIP(ifaceName string) (string, error) {
	if ifaceName != "" {
		iface, err := net.InterfaceByName(ifaceName)
		if err != nil {
			return "", err
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
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
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

func loadOrCreateUUID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return newUUID()
	}
	path := filepath.Join(home, ".config", "streambox", "uuid")
	if data, err := os.ReadFile(path); err == nil {
		if u := strings.TrimSpace(string(data)); u != "" {
			return u
		}
	}
	u := newUUID()
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	_ = os.WriteFile(path, []byte(u+"\n"), 0644)
	return u
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

func updateIDPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "streambox", "updateid")
}

func loadUpdateID() int64 {
	data, err := os.ReadFile(updateIDPath())
	if err != nil {
		return 0
	}
	id, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return id
}

func saveUpdateID(id int64) {
	_ = os.WriteFile(updateIDPath(), []byte(strconv.FormatInt(id, 10)+"\n"), 0644)
}

func expandHome(p string) string {
	if p == "~" || p == "~/" {
		home, _ := os.UserHomeDir()
		return home
	}
	if len(p) >= 2 && p[:2] == "~/" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}
