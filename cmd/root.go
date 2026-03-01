package cmd

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
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
	serve.Flags().StringP("media", "m", "", "Directory to serve video files from (required)")
	serve.Flags().IntP("port", "p", 8080, "HTTP port for the media server")
	serve.Flags().StringP("name", "n", "StreamBox", "Friendly device name shown on the TV")
	serve.Flags().StringP("iface", "i", "", "Network interface for SSDP (default: auto-detect)")
	_ = serve.MarkFlagRequired("media")
	rootCmd.AddCommand(serve)
}

func runServe(cmd *cobra.Command, args []string) error {
	mediaDir, _ := cmd.Flags().GetString("media")
	port, _ := cmd.Flags().GetInt("port")
	name, _ := cmd.Flags().GetString("name")
	ifaceName, _ := cmd.Flags().GetString("iface")

	lib, err := media.NewLibrary(mediaDir)
	if err != nil {
		return fmt.Errorf("scanning media directory: %w", err)
	}
	log.Printf("Found %d video files in %s", lib.VideoCount(), mediaDir)

	ip, err := detectIP(ifaceName)
	if err != nil {
		return fmt.Errorf("detecting local IP: %w", err)
	}
	log.Printf("Advertising as http://%s:%d", ip, port)

	uuid := newUUID()
	location := fmt.Sprintf("http://%s:%d/device.xml", ip, port)

	var iface *net.Interface
	if ifaceName != "" {
		iface, err = net.InterfaceByName(ifaceName)
		if err != nil {
			return fmt.Errorf("interface %q: %w", ifaceName, err)
		}
	}

	srv := server.New(server.Config{
		Port:    port,
		Name:    name,
		UUID:    uuid,
		IP:      ip,
		Library: lib,
	})

	ssdpSrv := ssdp.New(uuid, location, iface)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := ssdpSrv.Start(ctx); err != nil && ctx.Err() == nil {
			log.Printf("SSDP error: %v", err)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("Serving on :%d", port)
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
	// Dial an external UDP address to discover the outbound interface IP (no packet is sent).
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
