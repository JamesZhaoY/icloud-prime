package mail

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	imapclient "github.com/emersion/go-imap/client"
)

// fakeIMAP 返回一个预认证的 go-imap 客户端,服务端对所有命令回 OK。
func fakeIMAP(t *testing.T) (*imapclient.Client, net.Conn, net.Conn) {
	t.Helper()
	local, remote := net.Pipe()
	go func() {
		defer remote.Close()
		fmt.Fprint(remote, "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n")
		scanner := bufio.NewScanner(remote)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 2 {
				continue
			}
			if strings.EqualFold(fields[1], "LOGOUT") {
				fmt.Fprintf(remote, "* BYE bye\r\n%s OK done\r\n", fields[0])
				return
			}
			fmt.Fprintf(remote, "%s OK done\r\n", fields[0])
		}
	}()
	cli, err := imapclient.New(local)
	if err != nil {
		t.Fatal(err)
	}
	return cli, local, remote
}

type countingDialer struct {
	mu      sync.Mutex
	t       *testing.T
	count   int
	remotes []net.Conn
}

func (d *countingDialer) dial(string, string) (*imapclient.Client, net.Conn, error) {
	cli, local, remote := fakeIMAP(d.t)
	d.mu.Lock()
	d.count++
	d.remotes = append(d.remotes, remote)
	d.mu.Unlock()
	return cli, local, nil
}

func (d *countingDialer) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}

func testPool(t *testing.T) (*Pool, *countingDialer) {
	d := &countingDialer{t: t}
	p := NewPool()
	p.IdleTimeout = time.Hour
	p.dial = d.dial
	t.Cleanup(p.CloseIdle)
	return p, d
}

func TestPoolReusesConnection(t *testing.T) {
	p, d := testPool(t)
	for i := 0; i < 3; i++ {
		c := NewPooledClient("user@icloud.com", "pw", p)
		if err := c.Connect(); err != nil {
			t.Fatal(err)
		}
		c.Disconnect()
	}
	if d.dials() != 1 {
		t.Fatalf("expected one login for sequential requests, got %d", d.dials())
	}
}

func TestPoolSeparatesCredentials(t *testing.T) {
	p, d := testPool(t)
	for _, pw := range []string{"old", "new"} {
		c := NewPooledClient("user@icloud.com", pw, p)
		if err := c.Connect(); err != nil {
			t.Fatal(err)
		}
		c.Disconnect()
	}
	if d.dials() != 2 {
		t.Fatalf("a changed password must not reuse the old connection, got %d dials", d.dials())
	}
}

func TestPoolClearsCommandDeadline(t *testing.T) {
	p, d := testPool(t)
	c := NewPooledClient("user@icloud.com", "pw", p)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	c.cli.Timeout = 50 * time.Millisecond
	if err := c.cli.Noop(); err != nil {
		t.Fatal(err)
	}
	cli := c.cli
	c.Disconnect()
	time.Sleep(150 * time.Millisecond)
	select {
	case <-cli.LoggedOut():
		t.Fatal("idle pooled connection died from the leftover command deadline")
	default:
	}
	c = NewPooledClient("user@icloud.com", "pw", p)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect()
	if c.cli != cli || d.dials() != 1 {
		t.Fatalf("expected the idle connection to be reused, dials=%d", d.dials())
	}
}

func TestPoolDropsClosedConnection(t *testing.T) {
	p, d := testPool(t)
	c := NewPooledClient("user@icloud.com", "pw", p)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	cli := c.cli
	d.remotes[0].Close()
	select {
	case <-cli.LoggedOut():
	case <-time.After(time.Second):
		t.Fatal("client did not notice the closed connection")
	}
	c.Disconnect()
	c = NewPooledClient("user@icloud.com", "pw", p)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect()
	if d.dials() != 2 {
		t.Fatalf("closed connection must not be reused, dials=%d", d.dials())
	}
}

func TestPoolExpiresIdleConnections(t *testing.T) {
	p, d := testPool(t)
	now := time.Now()
	p.now = func() time.Time { return now }
	c := NewPooledClient("user@icloud.com", "pw", p)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	c.Disconnect()
	now = now.Add(2 * time.Hour)
	c = NewPooledClient("user@icloud.com", "pw", p)
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect()
	if d.dials() != 2 {
		t.Fatalf("expired idle connection must be replaced, dials=%d", d.dials())
	}
}

func TestPoolCapsIdleConnectionsAndForgets(t *testing.T) {
	p, d := testPool(t)
	p.MaxIdle = 2
	var clients []*Client
	for i := 0; i < 3; i++ {
		c := NewPooledClient("user@icloud.com", "pw", p)
		if err := c.Connect(); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	for _, c := range clients {
		c.Disconnect()
	}
	key := keyFor("user@icloud.com", "pw")
	p.mu.Lock()
	idle := len(p.idle[key])
	p.mu.Unlock()
	if idle != 2 || d.dials() != 3 {
		t.Fatalf("idle=%d dials=%d, want idle=2 dials=3", idle, d.dials())
	}
	p.Forget("user@icloud.com")
	p.mu.Lock()
	idle = len(p.idle[key])
	p.mu.Unlock()
	if idle != 0 {
		t.Fatalf("Forget left %d idle connections", idle)
	}
}
