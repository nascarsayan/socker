package socker

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrMuxClosed    = errors.New("mux has been closed")
	ErrNoAuthMethod = errors.New("no auth method can be applied to agent")
)

type (
	Matcher        func(addr string) bool
	MatcherBuilder func(string) (Matcher, error)
)

func MatchRegexp(addr string) (Matcher, error) {
	r, err := regexp.Compile(addr)
	if err != nil {
		return nil, err
	}
	return func(addr string) bool {
		return r.MatchString(addr)
	}, nil
}

func MatchIPNet(cidr string) (Matcher, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	return func(addr string) bool {
		if strings.IndexByte(addr, ':') >= 0 {
			host, _, err := net.SplitHostPort(addr)
			if err != nil || host == "" {
				return false
			}
			addr = host
		}

		ip := net.ParseIP(addr)
		if ip == nil {
			return false
		}
		return ipnet.Contains(ip)
	}, nil
}

func MatchPlain(addr string) (Matcher, error) {
	return func(dst string) bool {
		return addr == dst
	}, nil
}

type MuxAuth struct {
	Default *Auth
	Gates   map[string]string
	Agents  map[string]*Auth
}

func (a *MuxAuth) checkAuth(addr string, auth *Auth) error {
	_, err := auth.SSHConfig()
	if err != nil {
		if addr == "" {
			return err
		}
		return fmt.Errorf("%s: %s", addr, err.Error())
	}
	return nil
}

func (a *MuxAuth) checkAuthes(authes map[string]*Auth) error {
	for addr, auth := range authes {
		err := a.checkAuth(addr, auth)
		if err != nil {
			return err
		}
	}
	return nil
}

func (auth *MuxAuth) Validate() error {
	if auth.Default != nil {
		err := auth.checkAuth("", auth.Default)
		if err != nil {
			return err
		}
	} else if len(auth.Agents) == 0 {
		return ErrNoAuthMethod
	}

	return auth.checkAuthes(auth.Agents)
}

type muxAuth struct {
	Matcher
	*Auth
}

type muxGate struct {
	Matcher
	Gate string
}

type Mux struct {
	closed int32

	defaultAuth *Auth
	auths       []muxAuth
	gates       []muxGate

	mu   sync.RWMutex
	sshs map[string]*SSH

	aliveChan chan struct{}
}

func NewMux(auth MuxAuth, builder MatcherBuilder) (*Mux, error) {
	err := auth.Validate()
	if err != nil {
		return nil, err
	}
	var m Mux

	m.sshs = make(map[string]*SSH)

	m.gates = make([]muxGate, 0, len(auth.Gates))
	for addr, gate := range auth.Gates {
		if gate == "" {
			continue
		}
		matcher, err := builder(addr)
		if err != nil {
			return nil, fmt.Errorf("create matcher for addr %s failed: %s", addr, err.Error())
		}
		m.gates = append(m.gates, muxGate{
			Matcher: matcher,
			Gate:    gate,
		})
	}

	m.defaultAuth = auth.Default
	m.auths = make([]muxAuth, 0, len(auth.Agents))
	for addr, auth := range auth.Agents {
		matcher, err := builder(addr)
		if err != nil {
			return nil, fmt.Errorf("create matcher for addr %s failed: %s", addr, err.Error())
		}
		m.auths = append(m.auths, muxAuth{
			Matcher: matcher,
			Auth:    auth,
		})
	}
	return &m, nil
}

func (m *Mux) Keepalive(idle time.Duration) {
	m.aliveChan = make(chan struct{}, 1)
	go func() {
		var (
			timer    = time.NewTimer(idle)
			timerNil bool
		)

		for {
			select {
			case now := <-timer.C:
				if m.checkAlive(now, idle) {
					timer.Reset(idle)
				} else {
					timerNil = true
				}
			case _, ok := <-m.aliveChan:
				if !ok {
					if timer != nil {
						timer.Stop()
					}
					return
				}

				if timerNil {
					timer = time.NewTimer(idle)
					timerNil = false
				}
			}
		}
	}()
}

func (m *Mux) checkAlive(now time.Time, idle time.Duration) bool {
	var (
		sshs     []*SSH
		hasAlive bool
	)
	m.mu.Lock()
	for addr, s := range m.sshs {
		openAt, refs := s.Status()
		if refs <= 0 && now.Sub(openAt) >= idle {
			sshs = append(sshs, s)
			delete(m.sshs, addr)
		} else {
			hasAlive = true
		}
	}
	m.mu.Unlock()
	for _, s := range sshs {
		s.Close()
	}
	return hasAlive
}

func (m *Mux) markClosed() bool {
	return atomic.CompareAndSwapInt32(&m.closed, 0, 1)
}

func (m *Mux) isClosed() bool {
	return atomic.LoadInt32(&m.closed) == 1
}

func (m *Mux) Close() error {
	if !m.markClosed() {
		return nil
	}
	if m.aliveChan != nil {
		close(m.aliveChan)
	}
	m.mu.Lock()
	for _, s := range m.sshs {
		s.Close()
	}
	m.mu.Unlock()
	return nil
}

func (m *Mux) Gate(addr string) string {
	for i := range m.gates {
		if m.gates[i].Matcher(addr) {
			return m.gates[i].Gate
		}
	}
	return ""
}

func (m *Mux) Auth(addr string) (*Auth, error) {
	for i := range m.auths {
		if m.auths[i].Matcher(addr) {
			return m.auths[i].Auth, nil
		}
	}

	if m.defaultAuth != nil {
		return m.defaultAuth, nil
	}
	return nil, ErrNoAuthMethod
}

func (m *Mux) Dial(addr string) (*SSH, error) {
	if m.isClosed() {
		return nil, ErrMuxClosed
	}

	var (
		agent *SSH
		gate  *SSH
		has   bool

		err error
	)

	gateAddr := m.Gate(addr)
	m.mu.RLock()
	agent, has = m.sshs[addr]
	if !has {
		if gateAddr != "" {
			gate, has = m.sshs[gateAddr]
			if has {
				gate = gate.NopClose()
			}
		}
	} else {
		agent = agent.NopClose()
	}
	m.mu.RUnlock()
	if agent != nil {
		return agent, nil
	}

	if gate == nil && gateAddr != "" {
		gate, err = m.dial(gateAddr, nil)
		if err != nil {
			return nil, err
		}
	}
	if gate != nil {
		defer gate.Close()
	}

	return m.dial(addr, gate)
}

func (m *Mux) dial(addr string, gate *SSH) (*SSH, error) {
	auth, err := m.Auth(addr)
	if err != nil {
		return nil, err
	}

	agent, err := Dial(addr, auth.MustSSHConfig(), gate)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	tmp, has := m.sshs[addr]
	if has {
		agent, tmp = tmp, agent
	} else {
		m.sshs[addr] = agent
		if m.aliveChan != nil && !m.isClosed() {
			select {
			case m.aliveChan <- struct{}{}:
			default:
			}
		}
	}
	agent = agent.NopClose()
	m.mu.Unlock()

	if tmp != nil {
		tmp.Close()
	}
	return agent, nil
}
