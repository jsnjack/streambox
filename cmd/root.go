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
"syscall"

"github.com/spf13/cobra"
"streambox/internal/config"
"streambox/internal/media"
"streambox/internal/server"
"streambox/internal/ssdp"
)

var rootCmd = &cobra.Command{
Use:   "streambox",
Short: "Minimal DLNA media server for video files",
}

// Execute runs the CLI.
func Execute() {
if err := rootCmd.Execute(); err != nil {
os.Exit(1)
}
}

func init() {
serve := &cobra.Command{
Use:   "serve",
Short: "Start the DLNA media server",
RunE:  runServe,
}
serve.Flags().StringP("config", "c", "", "Path to TOML config file")
serve.Flags().StringP("media", "m", "", "Directory to serve video files from")
serve.Flags().IntP("port", "p", 0, "HTTP port (default 8080)")
serve.Flags().StringP("name", "n", "", "Friendly device name shown on the TV (default \"StreamBox\")")
serve.Flags().StringP("iface", "i", "", "Network interface for SSDP (default: auto-detect)")
serve.Flags().BoolP("debug", "d", false, "Enable debug logging")
rootCmd.AddCommand(serve)
}

func runServe(cmd *cobra.Command, args []string) error {
// Load config: start from defaults, then overlay TOML file, then CLI flags.
cfg := config.Defaults()

if cfgFile, _ := cmd.Flags().GetString("config"); cfgFile != "" {
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
if cfg.MediaDir == "" {
return fmt.Errorf("media directory is required (--media or config file media_dir)")
}
cfg.MediaDir = expandHome(cfg.MediaDir)

// Configure logging.
if cfg.LogFile != "" {
f, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
if err != nil {
return fmt.Errorf("opening log file %q: %w", cfg.LogFile, err)
}
defer f.Close()
log.SetOutput(io.MultiWriter(os.Stderr, f))
}
if cfg.Debug {
log.SetFlags(log.LstdFlags | log.Lshortfile)
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

uuid := newUUID()
location := fmt.Sprintf("http://%s:%d/device.xml", ip, cfg.Port)

var iface *net.Interface
if ifaceName != "" {
iface, err = net.InterfaceByName(ifaceName)
if err != nil {
return fmt.Errorf("interface %q: %w", ifaceName, err)
}
}

srv := server.New(server.Config{
Port:    cfg.Port,
Name:    cfg.Name,
UUID:    uuid,
IP:      ip,
Debug:   cfg.Debug,
Library: lib,
})

ssdpSrv := ssdp.New(uuid, location, iface, cfg.Debug)

ctx, cancel := context.WithCancel(context.Background())
defer cancel()

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
home, _ := os.UserHomeDir()
return home
}
if len(p) >= 2 && p[:2] == "~/" {
home, _ := os.UserHomeDir()
return filepath.Join(home, p[2:])
}
return p
}
