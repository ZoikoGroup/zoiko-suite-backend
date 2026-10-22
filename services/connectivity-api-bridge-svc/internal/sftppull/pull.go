// Package sftppull collects batch deliveries the platform pulls from a provider's SFTP server (XIC-05, §10.1).
//
// A pull is never a looser path than an upload: the destination comes from the connection's governed endpoint profile,
// is checked against egress policy and dialed through the pinned dialer, the server is authenticated only by a host key
// the contract pinned (never trusted on first use), the login is the connection's vault credential, and what comes back
// goes through exactly the verification an HTTPS delivery gets (batch.Service.IngestPulled).
//
// Remote layout: each batch is a directory under the inbox holding manifest.json and the files it lists. The provider
// writes the manifest LAST, so a directory without one is still being written and is left alone. A directory that has
// been taken (accepted, duplicate or refused with evidence) is moved under the done directory; one that could not be
// taken for a transient reason stays where it is for the next poll.
package sftppull

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"zoiko.io/connectivity-api-bridge-svc/internal/batch"
	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/egress"
)

// Source lists the live connections across tenants.
type Source interface {
	LiveRefs(ctx context.Context, limit int) ([]xicconn.DueRef, error)
}

// Ingester is what a pull hands a collected delivery to. *batch.Service satisfies it.
type Ingester interface {
	IngestPulled(ctx context.Context, conn *xicconn.Connection, v *registry.ContractVersion, op *registry.Operation, manifest []byte, files []batch.DeliveredFile) *batch.Outcome
}

// Config holds the puller's limits.
type Config struct {
	Inbox        string        // remote directory holding one directory per batch
	Done         string        // remote directory a taken batch is moved under; inside the inbox, because a provider account is usually chrooted and can write only there
	Timeout      time.Duration // the whole of one connection's pull
	MaxFileBytes int64         // the most read of any one file; one byte more is read so an oversize file is seen as one
	MaxManifest  int64
	MaxFiles     int // the most files read from one batch directory
	MaxBatches   int // the most batch directories taken from one connection per poll
	Port         int // used when the endpoint carries none
}

// DefaultConfig returns safe defaults.
func DefaultConfig() Config {
	return Config{Inbox: "inbox", Done: "inbox/processed", Timeout: 5 * time.Minute, MaxFileBytes: batch.DefaultConfig().MaxFileBytes, MaxManifest: 1 << 20,
		MaxFiles: 1000, MaxBatches: 20, Port: 22}
}

// Puller is the SFTP pull poller.
type Puller struct {
	Conns   batch.Connections
	Source  Source
	Batches Ingester
	Secrets inbound.Secrets
	Egress  egress.Policy
	Cfg     Config
	// Dial replaces the egress-pinned dialer. It exists for tests; production leaves it nil.
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	Clock   func() time.Time
	Observe func(kind, reason string)
}

func (p *Puller) now() time.Time {
	if p.Clock != nil {
		return p.Clock().UTC()
	}
	return time.Now().UTC()
}

func (p *Puller) cfg() Config {
	c := p.Cfg
	d := DefaultConfig()
	if c.Inbox == "" {
		c.Inbox = d.Inbox
	}
	if c.Done == "" {
		c.Done = d.Done
	}
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = d.MaxFileBytes
	}
	if c.MaxManifest <= 0 {
		c.MaxManifest = d.MaxManifest
	}
	if c.MaxFiles <= 0 {
		c.MaxFiles = d.MaxFiles
	}
	if c.MaxBatches <= 0 {
		c.MaxBatches = d.MaxBatches
	}
	if c.Port <= 0 {
		c.Port = d.Port
	}
	return c
}

func (p *Puller) observe(kind, reason string) {
	if p.Observe != nil {
		p.Observe(kind, reason)
	}
}

// Credential is the material of an SFTP credential in the vault: a user name and either a password or a private key.
type credential struct {
	Username   string `json:"username"`
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
}

func (c *credential) wipe() {
	c.Password, c.PrivateKey = "", ""
}

// PollOnce pulls every live SFTP connection once and returns how many batch directories were taken.
func (p *Puller) PollOnce(ctx context.Context) (int, error) {
	refs, err := p.Source.LiveRefs(ctx, 1000)
	if err != nil {
		return 0, err
	}
	total := 0
	var first error
	for _, ref := range refs {
		if ctx.Err() != nil {
			break
		}
		n, err := p.PullConnection(ctx, ref.TenantID, ref.ConnectionID)
		total += n
		if err != nil && first == nil {
			first = err
		}
	}
	return total, first
}

// Run polls on a ticker until ctx ends.
func (p *Puller) Run(ctx context.Context, interval time.Duration, report func(n int, err error)) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := p.PollOnce(ctx)
			if report != nil {
				report(n, err)
			}
		}
	}
}

// PullConnection collects what one connection's provider has waiting, for each of the contract's inbound SFTP
// operations the connection may run.
func (p *Puller) PullConnection(ctx context.Context, tenant, connectionID string) (int, error) {
	conn, err := p.Conns.Connection(ctx, connectionID)
	if err != nil || conn.TenantID != tenant {
		return 0, nil
	}
	v, err := p.Conns.PinnedContract(ctx, conn)
	if err != nil {
		return 0, nil
	}
	total := 0
	var first error
	for i := range v.Content.Operations {
		op := &v.Content.Operations[i]
		if op.Direction != registry.Inbound || op.Channel != "SFTP" || op.EndpointProfile == "" {
			continue
		}
		n, err := p.pullOperation(ctx, conn, v, op)
		total += n
		if err != nil && first == nil {
			first = err
		}
	}
	return total, first
}

func hostKeysOf(v *registry.ContractVersion, profile string) []string {
	for _, pr := range v.Content.Profiles {
		if pr.Kind == registry.ProfileEndpoint && pr.Name == profile {
			var s struct {
				HostKeys []string `json:"host_keys"`
			}
			if json.Unmarshal(pr.Spec, &s) == nil {
				return s.HostKeys
			}
		}
	}
	return nil
}

// hostKeyCallback accepts exactly the pinned host keys. A host whose key is not listed is refused.
func hostKeyCallback(pinned []string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		got := ssh.FingerprintSHA256(key)
		for _, want := range pinned {
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
				return nil
			}
		}
		return errors.New("the server's host key is not one the contract pinned")
	}
}

func (p *Puller) pullOperation(ctx context.Context, conn *xicconn.Connection, v *registry.ContractVersion, op *registry.Operation) (int, error) {
	cfg := p.cfg()
	dec, err := p.Conns.Operable(ctx, conn.TenantID, conn.ConnectionID, op.ID)
	if err != nil {
		return 0, nil // a connection that may not run the operation now is not pulled
	}
	pinned := hostKeysOf(v, op.EndpointProfile)
	if len(pinned) == 0 || dec.Host == "" || len(dec.CredentialRefs) == 0 {
		p.observe("pull_refused", "NOT_CONFIGURED")
		return 0, nil
	}
	hostport := dec.Host
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		hostport = net.JoinHostPort(strings.TrimSuffix(dec.Host, "."), strconv.Itoa(cfg.Port))
	}
	// the destination is judged before any credential is fetched; the pinned dialer judges the address again at connect
	if _, err := p.Egress.CheckDestination(ctx, "https://"+hostport); err != nil {
		p.observe("pull_refused", "EGRESS_DENIED")
		return 0, fmt.Errorf("sftp pull of %s: %w", conn.ConnectionID, err)
	}
	raw, err := p.Secrets.Secret(ctx, conn.TenantID, dec.CredentialRefs[0].Ref)
	if err != nil {
		p.observe("pull_refused", "CREDENTIAL_UNAVAILABLE")
		return 0, fmt.Errorf("sftp pull of %s: the credential could not be used", conn.ConnectionID)
	}
	var cred credential
	jerr := json.Unmarshal(raw, &cred)
	for i := range raw {
		raw[i] = 0
	}
	defer cred.wipe()
	if jerr != nil || cred.Username == "" {
		p.observe("pull_refused", "CREDENTIAL_SHAPE")
		return 0, fmt.Errorf("sftp pull of %s: the credential is not a user name with a password or a key", conn.ConnectionID)
	}
	auth, err := authMethod(&cred)
	if err != nil {
		p.observe("pull_refused", "CREDENTIAL_SHAPE")
		return 0, fmt.Errorf("sftp pull of %s: the credential is not usable", conn.ConnectionID)
	}

	cctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	client, closeAll, err := p.connect(cctx, hostport, cred.Username, auth, pinned)
	if err != nil {
		p.observe("pull_failed", "CONNECT")
		return 0, fmt.Errorf("sftp pull of %s: %w", conn.ConnectionID, err)
	}
	defer closeAll()
	return p.collect(cctx, client, cfg, conn, v, op)
}

func authMethod(c *credential) (ssh.AuthMethod, error) {
	switch {
	case c.PrivateKey != "":
		signer, err := ssh.ParsePrivateKey([]byte(c.PrivateKey))
		if err != nil {
			return nil, err
		}
		return ssh.PublicKeys(signer), nil
	case c.Password != "":
		return ssh.Password(c.Password), nil
	}
	return nil, errors.New("no password or key")
}

func (p *Puller) connect(ctx context.Context, hostport, user string, auth ssh.AuthMethod, pinned []string) (*sftp.Client, func(), error) {
	dial := p.Dial
	if dial == nil {
		dial = p.Egress.DialContext(nil)
	}
	nc, err := dial(ctx, "tcp", hostport)
	if err != nil {
		return nil, nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}
	sc, chans, reqs, err := ssh.NewClientConn(nc, hostport, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{auth},
		HostKeyCallback: hostKeyCallback(pinned), Timeout: 30 * time.Second})
	if err != nil {
		_ = nc.Close()
		return nil, nil, err
	}
	sshc := ssh.NewClient(sc, chans, reqs)
	cl, err := sftp.NewClient(sshc)
	if err != nil {
		_ = sshc.Close()
		return nil, nil, err
	}
	// the connection goes first so a server that never answers the goodbye cannot hold the poller
	return cl, func() { _ = sshc.Close(); _ = cl.Close() }, nil
}

func (p *Puller) collect(ctx context.Context, cl *sftp.Client, cfg Config, conn *xicconn.Connection, v *registry.ContractVersion, op *registry.Operation) (int, error) {
	entries, err := cl.ReadDir(cfg.Inbox)
	if err != nil {
		return 0, fmt.Errorf("sftp pull of %s: the inbox could not be listed", conn.ConnectionID)
	}
	var dirs []string
	for _, e := range entries {
		// the directory taken batches are moved to lives inside the inbox and is never itself a batch
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && path.Join(cfg.Inbox, e.Name()) != path.Clean(cfg.Done) {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	taken := 0
	var first error
	for _, d := range dirs {
		if ctx.Err() != nil || taken >= cfg.MaxBatches {
			break
		}
		ok, err := p.takeDir(ctx, cl, cfg, conn, v, op, d)
		if ok {
			taken++
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return taken, first
}

func readCapped(cl *sftp.Client, name string, limit int64) ([]byte, error) {
	f, err := cl.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit+1))
}

// takeDir reads one batch directory and hands it to the engine. It reports whether the directory was taken (and so
// moved out of the inbox).
func (p *Puller) takeDir(ctx context.Context, cl *sftp.Client, cfg Config, conn *xicconn.Connection, v *registry.ContractVersion, op *registry.Operation, dir string) (bool, error) {
	base := path.Join(cfg.Inbox, dir)
	manifest, err := readCapped(cl, path.Join(base, "manifest.json"), cfg.MaxManifest)
	if err != nil {
		return false, nil // still being written: the manifest is the provider's last step
	}
	if int64(len(manifest)) > cfg.MaxManifest {
		p.observe("pull_refused", "MANIFEST_TOO_LARGE")
		return p.finish(cl, cfg, dir, "rejected"), nil
	}
	// the files the manifest names are the files read: nothing else in the directory is opened
	var m struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	if json.Unmarshal(manifest, &m) != nil || len(m.Files) == 0 || len(m.Files) > cfg.MaxFiles {
		// an unreadable manifest still goes to the engine, which refuses it with evidence
		m.Files = nil
	}
	var files []batch.DeliveredFile
	for _, f := range m.Files {
		if f.Name == "" || f.Name != path.Base(f.Name) || strings.HasPrefix(f.Name, ".") {
			continue // a name that reaches outside the directory is never opened; the engine refuses the batch
		}
		data, err := readCapped(cl, path.Join(base, f.Name), cfg.MaxFileBytes)
		if err != nil {
			return false, fmt.Errorf("sftp pull of %s: a file could not be read", conn.ConnectionID)
		}
		files = append(files, batch.DeliveredFile{Name: f.Name, Data: data})
	}
	o := p.Batches.IngestPulled(ctx, conn, v, op, manifest, files)
	switch {
	case o.Status >= 500:
		p.observe("pull_deferred", "TRANSIENT")
		return false, nil
	case o.Status == 202 || o.Duplicate:
		p.observe("pull_taken", "ACCEPTED")
		return p.finish(cl, cfg, dir, "taken"), nil
	default:
		p.observe("pull_taken", "REFUSED")
		return p.finish(cl, cfg, dir, "rejected"), nil
	}
}

// finish moves a taken directory out of the inbox so it is not read again. It reports whether the move happened.
func (p *Puller) finish(cl *sftp.Client, cfg Config, dir, outcome string) bool {
	_ = cl.MkdirAll(cfg.Done)
	dst := path.Join(cfg.Done, dir+"."+outcome+"."+strconv.FormatInt(p.now().Unix(), 10))
	return cl.PosixRename(path.Join(cfg.Inbox, dir), dst) == nil || cl.Rename(path.Join(cfg.Inbox, dir), dst) == nil
}
