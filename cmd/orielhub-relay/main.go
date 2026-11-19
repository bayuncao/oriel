// orielhub-relay 是遠程操控的中繼服務（ADR-37）。它只說 HTTP；TLS 由前面
// 的反向代理負責（部署見 D148）。它不存任何狀態：房間號由 Mac 的房間密
// 鑰算出，重啟後 Mac 與 App 重連即可。
//
//	orielhub-relay -addr :8080
//
// 連線上限（RL08–RL15）：-max-conns 是全局的，含還沒送 Hello 的連線；一條
// 連線最多積壓約 4 MiB，所以按機器記憶體調它。-max-conns-per-ip 是每個客戶
// 端地址的（IPv6 按 /64）。在反向代理後面時一定要給 -client-ip-header（代理
// 寫入客戶端地址的標頭，如 X-Forwarded-For），否則所有連線都算成代理一個地
// 址，超過 -max-conns-per-ip 就被拒。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bayuncao/oriel/internal/relay"
	"github.com/bayuncao/oriel/internal/relay/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	maxApps := flag.Int("max-apps", 16, "apps connected to one room at a time")
	maxConns := flag.Int("max-conns", 1024, "connections to the relay at a time; each can hold about 4 MiB of queued frames")
	maxPerIP := flag.Int("max-conns-per-ip", 32, "connections from one client address at a time (an IPv6 /64 counts as one address)")
	ipHeader := flag.String("client-ip-header", "", "header in which the reverse proxy puts the client address, e.g. X-Forwarded-For; required behind a proxy")
	flag.Parse()
	for name, v := range map[string]int{"max-apps": *maxApps, "max-conns": *maxConns, "max-conns-per-ip": *maxPerIP} {
		if v < 1 {
			fmt.Fprintf(os.Stderr, "orielhub-relay: -%s must be at least 1\n", name)
			os.Exit(2)
		}
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	o := server.Options{Logger: log, MaxApps: *maxApps, MaxConns: *maxConns, MaxConnsPerIP: *maxPerIP, ClientIPHeader: *ipHeader}
	if err := run(log, *addr, o); err != nil {
		log.Error("relay: exit", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, addr string, o server.Options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	relaySrv := server.New(o)
	mux := http.NewServeMux()
	mux.Handle(relay.Path, relaySrv)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Info("relay: listening", "addr", addr)

	select {
	case err := <-errc:
		relaySrv.Close()
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := httpSrv.Shutdown(shutdown) // WebSocket 是 hijack 的連線，Shutdown 不等它們
	relaySrv.Close()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
