package mail

import (
	"crypto/sha256"
	"net"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

const (
	// DefaultPoolIdleTimeout 是空闲 IMAP 连接的保留时长。
	DefaultPoolIdleTimeout = 3 * time.Minute
	// DefaultPoolMaxIdle 是每个账号最多保留的空闲连接数。
	DefaultPoolMaxIdle = 2
	// poolProbeAfter 空闲超过该时长的连接在复用前先发 NOOP 确认可用。
	poolProbeAfter   = 20 * time.Second
	poolProbeTimeout = 5 * time.Second
)

// Pool 复用已登录的 IMAP 连接,避免每个请求都重新 TLS 握手 + LOGIN。
//
// go-imap 的客户端不支持并发命令,所以连接在借出期间只归一个 Client 使用;
// 同一账号的并发请求会各自拿到不同连接,并发度与不使用连接池时一致。
type Pool struct {
	IdleTimeout time.Duration
	MaxIdle     int

	mu    sync.Mutex
	idle  map[poolKey][]idleConn
	timer *time.Timer
	dial  func(appleID, appPassword string) (*client.Client, net.Conn, error)
	now   func() time.Time
}

type poolKey struct {
	appleID  string
	password [sha256.Size]byte
}

type idleConn struct {
	cli      *client.Client
	raw      net.Conn // 底层 TCP 连接,用于清除 go-imap 留下的读超时
	returned time.Time
}

// NewPool 创建连接池。
func NewPool() *Pool {
	return &Pool{
		IdleTimeout: DefaultPoolIdleTimeout,
		MaxIdle:     DefaultPoolMaxIdle,
		idle:        make(map[poolKey][]idleConn),
		dial:        dialIMAP,
		now:         time.Now,
	}
}

func keyFor(appleID, appPassword string) poolKey {
	return poolKey{appleID: appleID, password: sha256.Sum256([]byte(appPassword))}
}

func alive(cli *client.Client) bool {
	select {
	case <-cli.LoggedOut():
		return false
	default:
	}
	return cli.State()&imap.AuthenticatedState != 0
}

// get 返回可用连接:优先复用空闲连接,否则新建并登录。
func (p *Pool) get(appleID, appPassword string) (*client.Client, net.Conn, error) {
	key := keyFor(appleID, appPassword)
	for {
		p.mu.Lock()
		conns := p.idle[key]
		if len(conns) == 0 {
			p.mu.Unlock()
			break
		}
		conn := conns[len(conns)-1]
		p.idle[key] = conns[:len(conns)-1]
		if len(p.idle[key]) == 0 {
			delete(p.idle, key)
		}
		now := p.now()
		p.mu.Unlock()

		if !alive(conn.cli) || now.Sub(conn.returned) > p.idleTimeout() {
			closeConn(conn.cli)
			continue
		}
		if now.Sub(conn.returned) > poolProbeAfter && !probe(conn.cli) {
			closeConn(conn.cli)
			continue
		}
		return conn.cli, conn.raw, nil
	}
	return p.dial(appleID, appPassword)
}

// put 归还连接;连接已断开或空闲数已满时直接关闭。
func (p *Pool) put(appleID, appPassword string, cli *client.Client, raw net.Conn) {
	if cli == nil {
		return
	}
	if raw == nil || !alive(cli) {
		closeConn(cli)
		return
	}
	// go-imap 每条命令都把 deadline 设为 now+Timeout 且不清除;不清掉的话,
	// 后台读协程会在空闲 Timeout 后超时退出,池里的连接随之失效。
	if err := raw.SetDeadline(time.Time{}); err != nil {
		closeConn(cli)
		return
	}
	key := keyFor(appleID, appPassword)
	p.mu.Lock()
	if len(p.idle[key]) >= p.maxIdle() {
		p.mu.Unlock()
		closeConn(cli)
		return
	}
	p.idle[key] = append(p.idle[key], idleConn{cli: cli, raw: raw, returned: p.now()})
	if p.timer == nil {
		p.timer = time.AfterFunc(p.idleTimeout(), p.sweep)
	}
	p.mu.Unlock()
}

// sweep 关闭超时的空闲连接;仍有空闲连接时重新定时。
func (p *Pool) sweep() {
	p.mu.Lock()
	now := p.now()
	var expired []*client.Client
	for key, conns := range p.idle {
		kept := conns[:0]
		for _, conn := range conns {
			if now.Sub(conn.returned) >= p.idleTimeout() {
				expired = append(expired, conn.cli)
			} else {
				kept = append(kept, conn)
			}
		}
		if len(kept) == 0 {
			delete(p.idle, key)
		} else {
			p.idle[key] = kept
		}
	}
	p.timer = nil
	if len(p.idle) > 0 {
		p.timer = time.AfterFunc(p.idleTimeout(), p.sweep)
	}
	p.mu.Unlock()
	for _, cli := range expired {
		closeConn(cli)
	}
}

// Forget 关闭指定 Apple ID 的全部空闲连接(账号删除或改密码时调用)。
func (p *Pool) Forget(appleID string) {
	p.closeWhere(func(key poolKey) bool { return key.appleID == appleID })
}

// CloseIdle 关闭全部空闲连接。
func (p *Pool) CloseIdle() {
	p.closeWhere(func(poolKey) bool { return true })
}

func (p *Pool) closeWhere(match func(poolKey) bool) {
	p.mu.Lock()
	var closing []*client.Client
	for key, conns := range p.idle {
		if !match(key) {
			continue
		}
		for _, conn := range conns {
			closing = append(closing, conn.cli)
		}
		delete(p.idle, key)
	}
	p.mu.Unlock()
	for _, cli := range closing {
		closeConn(cli)
	}
}

func (p *Pool) idleTimeout() time.Duration {
	if p.IdleTimeout > 0 {
		return p.IdleTimeout
	}
	return DefaultPoolIdleTimeout
}

func (p *Pool) maxIdle() int {
	if p.MaxIdle > 0 {
		return p.MaxIdle
	}
	return DefaultPoolMaxIdle
}

// probe 用短超时发 NOOP,避免在已失效的 TCP 连接上卡满 20 秒命令超时。
func probe(cli *client.Client) bool {
	timeout := cli.Timeout
	cli.Timeout = poolProbeTimeout
	err := cli.Noop()
	cli.Timeout = timeout
	return err == nil
}

func closeConn(cli *client.Client) {
	go func() {
		cli.Timeout = poolProbeTimeout
		if err := cli.Logout(); err != nil {
			_ = cli.Terminate()
		}
	}()
}
