package health

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// When every node stops answering at once, the interesting question is
// which end the fault is. Five nodes in five countries do not usually go
// together; a controller whose own network has gone does. Without asking,
// ForgeSync records five node failures for one local fault, and the
// history fills with the wrong thing while the real one is not said at
// all.
//
// So when nothing answers, ask something that is not a Forgejo node. What
// is asked is deliberately not ICMP: the systemd unit runs with an empty
// capability set, and ping needs CAP_NET_RAW. A DNS query over UDP is
// cheap, needs no privilege, and fails the same way a controller's own
// traffic would.
//
// It is only asked when everything has already gone quiet, so there is no
// steady beacon to a third party from an installation that is working.

// Uplink says whether this controller can reach anything at all.
type Uplink interface {
	// Up reports whether any reference answered. A prober that cannot
	// tell says true: refusing to believe a node is down because a
	// resolver was slow would be worse than the noise.
	Up(ctx context.Context) bool
}

// Resolvers asks several public DNS resolvers and is satisfied by any one
// of them, so one provider being down, blocked or slow is not mistaken
// for the network being gone.
type Resolvers struct {
	// Addresses are host:port; a bare address gets :53.
	Addresses []string
	// Name is what to look up. It only has to be something that resolves.
	Name string
	// Timeout bounds the whole round, not each resolver.
	Timeout time.Duration
}

// NewResolvers makes a prober from a list of addresses. An empty list
// turns the check off, which is what nil means here.
func NewResolvers(addresses []string, timeout time.Duration) Uplink {
	var addrs []string
	for _, a := range addresses {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if !strings.Contains(a, ":") || strings.Count(a, ":") > 1 && !strings.Contains(a, "]") {
			a = net.JoinHostPort(a, "53")
		}
		addrs = append(addrs, a)
	}
	if len(addrs) == 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Resolvers{Addresses: addrs, Name: "example.org.", Timeout: timeout}
}

// Up asks all of them at once and returns as soon as one answers.
func (r *Resolvers) Up(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	answered := make(chan bool, len(r.Addresses))
	var wg sync.WaitGroup
	for _, addr := range r.Addresses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "udp", addr)
				},
			}
			// Any answer at all will do, including "no such name": what is
			// being asked is whether anything out there is listening.
			_, err := res.LookupHost(ctx, r.Name)
			var dnsErr *net.DNSError
			ok := err == nil
			if !ok && asDNSError(err, &dnsErr) {
				ok = dnsErr.IsNotFound
			}
			answered <- ok
		}()
	}
	go func() { wg.Wait(); close(answered) }()

	for ok := range answered {
		if ok {
			return true
		}
	}
	return false
}

func asDNSError(err error, target **net.DNSError) bool {
	for err != nil {
		if e, ok := err.(*net.DNSError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
