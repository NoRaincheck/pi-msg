package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/grandcat/zeroconf"
)

func escapeInstance(name string) string {
	r := strings.NewReplacer(`@`, `\@`, `.`, `\.`, `,`, `\,`, `"`, `\"`, `\`, `\\`)
	return r.Replace(name)
}

func main() {
	txt := []string{"txtvers=1", "port=5298", "status=avail", "nick=Pi"}
	svc, err := zeroconf.Register(escapeInstance("pi-bot@mbpro"), "_presence._tcp", "local", 5298, txt, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		os.Exit(1)
	}
	fmt.Println("registered pi-bot@mbpro on _presence._tcp with TXT:", txt)
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	svc.Shutdown()
}
