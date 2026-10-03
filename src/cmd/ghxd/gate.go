package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/brunoborges/ghx/src/internal/governor"
)

func runGate(args []string) {
	flags := flag.NewFlagSet("gate", flag.ExitOnError)
	dir := flags.String("dir", "", "private state directory")
	addr := flags.String("addr", "127.0.0.1:18771", "loopback listener")
	flags.Parse(args)
	g, err := governor.New(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Gate policy and credential material are installed locally by devmsg.
	g.Invalidate = func() {
		path := filepath.Join(*dir, "epoch")
		tmp := path + ".tmp"
		if os.WriteFile(tmp, []byte(fmt.Sprint(time.Now().UnixNano())), 0644) == nil {
			os.Rename(tmp, path)
		}
	}
	ln, err := g.Start(*addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer ln.Close()
	json.NewEncoder(os.Stdout).Encode(map[string]any{"gate": "ready", "box_hourly": g.Policy.BoxHourly})
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(ch)
	<-ch
}
