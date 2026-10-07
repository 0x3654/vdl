// restart.go — рестарт контейнера cobalt через Docker API (unix-сокет):
// хук после получения новых кук в TG (cobalt читает cookies.json только
// при старте). Никаких кронов: перезапуск ровно в момент смены кук.
package cobalt

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// Docker — тонкий клиент Docker Engine API поверх unix-сокета.
type Docker struct {
	Sock string // путь к сокету, дефолт /var/run/docker.sock
	HC   *http.Client
}

// Restart перезапускает контейнер (t — grace-период в секундах).
func (d *Docker) Restart(ctx context.Context, container string, t int) error {
	sock := d.Sock
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	hc := d.HC
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var nd net.Dialer
					return nd.DialContext(ctx, "unix", sock)
				},
			}}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://docker/containers/%s/restart?t=%d", container, t), nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		return nil
	}
	if resp.StatusCode == 404 {
		return fmt.Errorf("контейнер %s не найден", container)
	}
	return fmt.Errorf("docker api: HTTP %d", resp.StatusCode)
}

// RestartFromEnv — обёртка для хука: имя контейнера из env COBALT_CONTAINER.
func RestartFromEnv(ctx context.Context) error {
	name := os.Getenv("COBALT_CONTAINER")
	if name == "" {
		name = "vdl-cobalt"
	}
	return (&Docker{}).Restart(ctx, name, 5)
}
