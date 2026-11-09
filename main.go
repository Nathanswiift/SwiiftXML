// SwiiftXML is a viewer for XML files that are too large to open in normal
// editors. It runs as a small local web server and shows its user interface
// in the browser; nothing is installed and no data leaves the computer.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

var version = "1.0.0"

func main() {
	port := flag.Int("port", 0, "port to listen on (default: any free port)")
	noBrowser := flag.Bool("no-browser", false, "do not open the browser automatically")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "SwiiftXML %s - browse very large XML files\n\nUsage: swiiftxml [options] [file.xml]\n\nOptions:\n", version)
		flag.PrintDefaults()
	}
	flag.Parse()

	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(*port))
	if err != nil {
		fatal("Could not start the local server: %v", err)
	}
	tok := make([]byte, 16)
	rand.Read(tok)
	a := &app{
		token: hex.EncodeToString(tok),
		host:  ln.Addr().String(),
		quit:  make(chan struct{}),
	}
	if path := flag.Arg(0); path != "" {
		if err := a.openFile(cleanPath(path)); err != nil {
			a.notice = fmt.Sprintf("Could not open %s: %v", path, friendly(err))
		}
	}

	url := fmt.Sprintf("http://%s/?t=%s", a.host, a.token)
	fmt.Printf("\n  SwiiftXML %s is running.\n\n", version)
	fmt.Printf("  Your browser should open automatically. If it does not, open this address:\n\n      %s\n\n", url)
	fmt.Printf("  Close this window (or press Ctrl+C) to stop SwiiftXML.\n\n")

	srv := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-a.quit
		srv.Close()
	}()
	if !*noBrowser {
		go openBrowser(url)
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fatal("Server error: %v", err)
	}
	fmt.Println("  SwiiftXML has stopped.")
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err == nil {
		go cmd.Wait()
	}
}

// fatal prints an error and, on Windows where the console window would
// otherwise close immediately, waits for Enter so the message can be read.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n  "+format+"\n\n", args...)
	if runtime.GOOS == "windows" {
		fmt.Fprint(os.Stderr, "  Press Enter to close this window.")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(1)
}
