package store

// G-07 evidence ledger: verify the hash chain, export it, seal it into
// packages and re-check those packages. The database links every decision
// (000016 gdl_chain_link); everything here only READS the chain and re-computes
// it, apart from SealPackage, which appends one package row.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/ledger"
)

// CanonicalFormDescription tells an offline verifier how to re-compute a link
// from an export.
const CanonicalFormDescription = "curr_hash = sha256(prev_hash || utf8(canonical)); canonical is the decision's " +
	"PostgreSQL jsonb text (gdl_canonical v1, carried in each exported decision). The first decision's prev_hash " +
	"is 32 zero bytes. A package's merkle_root is the RFC 6962 Merkle Tree Hash over curr_hash of decisions " +
	"1..to_seq; package_hash = sha256(prev_package_hash || merkle_root || head_hash || uint64be(to_seq))."

// ChainExport is the evidence bundle GET /v1/evidence/chain/export returns:
// every decision with its canonical form, every package, every replay, and
// the verification result at the time of export.
type ChainExport struct {
	Format          string                    `json:"format"`
	TenantID        string                    `json:"tenant_id"`
	RetentionClass  string                    `json:"retention_class"`
	RecordClass     string                    `json:"record_class"`
	ExportedAt      time.Time                 `json:"exported_at"`
	CanonicalForm   string                    `json:"canonical_form"`
	Verification    *domain.ChainVerification `json:"verification"`
	Decisions       []ExportedDecision        `json:"decisions"`
	Packages        []*domain.EvidencePackage `json:"packages"`
	ReplayManifests []*domain.ReplayManifest  `json:"replay_manifests"`
}

// ExportedDecision is a stored decision with the exact canonical text its
// curr_hash was computed over.
type ExportedDecision struct {
	*domain.GovernanceDecision
	Canonical string `json:"canonical"`
}

// linkRow is one decision's chain entry, as stored.
type linkRow struct {
	seq       int64
	id        string
	prev      []byte
	curr      []byte
	canonical string
}

// verifyLinks walks rows in chain order: contiguous sequence from 1, each
// prev_hash equal to the previous curr_hash, each curr_hash re-computing.
// It reports the first break and how many links were checked.
func verifyLinks(rows []linkRow) (int64, *domain.ChainBreak) {
	expectPrev := ledger.Genesis
	for i, r := range rows {
		var reason string
		switch {
		case r.seq != int64(i+1):
			reason = "SEQUENCE_GAP"
		case !bytesEqual(r.prev, expectPrev):
			reason = "PREV_HASH_MISMATCH"
		case !ledger.LinkValid(r.prev, r.curr, r.canonical):
			reason = "HASH_MISMATCH"
		}
		if reason != "" {
			return int64(i + 1), &domain.ChainBreak{ChainSeq: r.seq, DecisionID: r.id, Reason: reason}
		}
		expectPrev = r.curr
	}
	return int64(len(rows)), nil
}

func bytesEqual(a, b []byte) bool { return hex.EncodeToString(a) == hex.EncodeToString(b) }

// checkPackage re-computes one package from the stored leaves (curr_hash of
// decisions 1..) and the package before it.
func checkPackage(p *domain.EvidencePackage, leaves [][]byte, prevPackageHash []byte) domain.PackageCheck {
	c := domain.PackageCheck{PackageID: p.PackageID, PackageSeq: p.PackageSeq, ToSeq: p.ToSeq}
	if p.ToSeq < 1 || p.ToSeq > int64(len(leaves)) {
		c.Reason = "LEAVES_MISSING"
		return c
	}
	root := ledger.MerkleRoot(leaves[:p.ToSeq])
	c.RecomputedRoot = hex.EncodeToString(root)
	head := leaves[p.ToSeq-1]
	switch {
	case c.RecomputedRoot != p.MerkleRoot:
		c.Reason = "ROOT_MISMATCH"
	case hex.EncodeToString(head) != p.HeadHash:
		c.Reason = "HEAD_MISMATCH"
	case hex.EncodeToString(prevPackageHash) != p.PrevPackageHash,
		hex.EncodeToString(ledger.PackageHash(prevPackageHash, root, head, p.ToSeq)) != p.PackageHash:
		c.Reason = "PACKAGE_CHAIN_MISMATCH"
	default:
		c.Valid = true
	}
	return c
}

// readLinks reads the tenant's chain, 1..throughSeq (all when throughSeq <= 0).
func readLinks(ctx context.Context, tx pgx.Tx, tenantID string, throughSeq int64) ([]linkRow, error) {
	q := `SELECT g.chain_seq, g.decision_id, g.prev_hash, g.curr_hash, gdl_canonical(g)
	        FROM governance_decisions g
	       WHERE g.tenant_id = $1 AND ($2 <= 0 OR g.chain_seq <= $2)
	       ORDER BY g.chain_seq`
	rows, err := tx.Query(ctx, q, tenantID, throughSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []linkRow
	for rows.Next() {
		var r linkRow
		if err := rows.Scan(&r.seq, &r.id, &r.prev, &r.curr, &r.canonical); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const packageColumns = `package_id::text, tenant_id, package_seq, to_seq, leaf_count, merkle_root, head_hash,
	head_decision_id, prev_package_hash, package_hash, label, reason, sealed_by, sealed_at,
	idempotency_key, request_hash`

func scanPackage(row pgx.Row) (*domain.EvidencePackage, error) {
	p := &domain.EvidencePackage{}
	var root, head, prev, hash []byte
	if err := row.Scan(&p.PackageID, &p.TenantID, &p.PackageSeq, &p.ToSeq, &p.LeafCount, &root, &head,
		&p.HeadDecisionID, &prev, &hash, &p.Label, &p.Reason, &p.SealedBy, &p.SealedAt,
		&p.IdempotencyKey, &p.RequestHash); err != nil {
		return nil, err
	}
	p.MerkleRoot = hex.EncodeToString(root)
	p.HeadHash = hex.EncodeToString(head)
	p.PrevPackageHash = hex.EncodeToString(prev)
	p.PackageHash = hex.EncodeToString(hash)
	return p, nil
}

func readPackages(ctx context.Context, tx pgx.Tx, tenantID string) ([]*domain.EvidencePackage, error) {
	rows, err := tx.Query(ctx, `SELECT `+packageColumns+` FROM evidence_packages WHERE tenant_id = $1 ORDER BY package_seq`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.EvidencePackage
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// verify builds a ChainVerification from the links and packages read.
func verify(links []linkRow, packages []*domain.EvidencePackage) *domain.ChainVerification {
	v := &domain.ChainVerification{VerifiedAt: time.Now().UTC(), Packages: []domain.PackageCheck{}}
	v.DecisionsChecked, v.FirstBreak = verifyLinks(links)
	if n := len(links); n > 0 {
		v.HeadSeq = links[n-1].seq
		v.HeadHash = hex.EncodeToString(links[n-1].curr)
		v.HeadDecisionID = links[n-1].id
	}
	leaves := make([][]byte, len(links))
	for i, l := range links {
		leaves[i] = l.curr
	}
	prev := ledger.Genesis
	allPackages := true
	for _, p := range packages {
		c := checkPackage(p, leaves, prev)
		v.Packages = append(v.Packages, c)
		allPackages = allPackages && c.Valid
		if raw, err := hex.DecodeString(p.PackageHash); err == nil {
			prev = raw
		}
	}
	v.PackagesChecked = len(packages)
	v.Valid = v.FirstBreak == nil && allPackages
	return v
}

// VerifyChain walks the tenant's whole chain and re-computes every package.
func (s *PgStore) VerifyChain(ctx context.Context, tenantID string) (*domain.ChainVerification, error) {
	var v *domain.ChainVerification
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		links, err := readLinks(ctx, tx, tenantID, 0)
		if err != nil {
			return err
		}
		packages, err := readPackages(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		v = verify(links, packages)
		return nil
	})
	if err != nil {
		return nil, s.unavailable("VerifyChain", err, zap.String("tenant_id", tenantID))
	}
	return v, nil
}

// ExportChain reads the tenant's whole ledger in one transaction, so the
// bundle is one consistent snapshot.
func (s *PgStore) ExportChain(ctx context.Context, tenantID string) (*ChainExport, error) {
	out := &ChainExport{
		Format:          "zoiko.governance-decision-log.evidence-bundle/v1",
		TenantID:        tenantID,
		RetentionClass:  domain.RetentionClass,
		RecordClass:     domain.RecordClass,
		ExportedAt:      time.Now().UTC(),
		CanonicalForm:   CanonicalFormDescription,
		Decisions:       []ExportedDecision{},
		Packages:        []*domain.EvidencePackage{},
		ReplayManifests: []*domain.ReplayManifest{},
	}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+decisionColumns+`, gdl_canonical(g)
			FROM governance_decisions g WHERE g.tenant_id = $1 ORDER BY g.chain_seq`, tenantID)
		if err != nil {
			return err
		}
		var links []linkRow
		for rows.Next() {
			var canonical string
			d, err := scanDecisionPlus(rows, &canonical)
			if err != nil {
				rows.Close()
				return err
			}
			prev, _ := hex.DecodeString(d.PrevHash)
			curr, _ := hex.DecodeString(d.CurrHash)
			links = append(links, linkRow{seq: d.ChainSeq, id: d.DecisionID, prev: prev, curr: curr, canonical: canonical})
			out.Decisions = append(out.Decisions, ExportedDecision{GovernanceDecision: d, Canonical: canonical})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		packages, err := readPackages(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if packages != nil {
			out.Packages = packages
		}
		out.Verification = verify(links, packages)

		mrows, err := tx.Query(ctx, `SELECT `+replayManifestColumns+` FROM replay_manifests WHERE tenant_id = $1 ORDER BY replayed_at`, tenantID)
		if err != nil {
			return err
		}
		defer mrows.Close()
		for mrows.Next() {
			m, err := scanReplayManifest(mrows)
			if err != nil {
				return err
			}
			out.ReplayManifests = append(out.ReplayManifests, m)
		}
		return mrows.Err()
	})
	if err != nil {
		return nil, s.unavailable("ExportChain", err, zap.String("tenant_id", tenantID))
	}
	return out, nil
}

// scanDecisionPlus scans decisionColumns followed by extra columns.
func scanDecisionPlus(row pgx.Row, extra ...any) (*domain.GovernanceDecision, error) {
	return scanDecision(extendedRow{row: row, extra: extra})
}

// extendedRow appends extra scan targets after scanDecision's own.
type extendedRow struct {
	row   pgx.Row
	extra []any
}

func (e extendedRow) Scan(dest ...any) error { return e.row.Scan(append(dest, e.extra...)...) }

// SealPackage appends a package sealing the chain through the named decision
// (the head when none is named). One sealer per tenant at a time; a retry with
// the same Idempotency-Key returns the package the first call sealed.
func (s *PgStore) SealPackage(ctx context.Context, req SealRequest) (*domain.EvidencePackage, bool, error) {
	var pkg *domain.EvidencePackage
	created := false
	err := s.withRLS(ctx, req.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('gdl-seal:' || $1, 0))`, req.TenantID); err != nil {
			return err
		}

		existing, err := scanPackage(tx.QueryRow(ctx,
			`SELECT `+packageColumns+` FROM evidence_packages WHERE tenant_id = $1 AND idempotency_key = $2`,
			req.TenantID, req.IdempotencyKey))
		if err == nil {
			pkg = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var toSeq int64
		if req.ThroughDecisionID != "" {
			if err := tx.QueryRow(ctx,
				`SELECT chain_seq FROM governance_decisions WHERE tenant_id = $1 AND decision_id = $2`,
				req.TenantID, req.ThroughDecisionID).Scan(&toSeq); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.ErrDecisionNotFound
				}
				return err
			}
		} else if err := tx.QueryRow(ctx,
			`SELECT COALESCE(max(chain_seq), 0) FROM governance_decisions WHERE tenant_id = $1`,
			req.TenantID).Scan(&toSeq); err != nil {
			return err
		}
		if toSeq == 0 {
			return domain.ErrNothingToSeal
		}

		// Scanned into a fresh slice, never into ledger.Genesis: a driver that
		// reuses the destination's buffer would overwrite the shared genesis.
		lastSeq, lastTo := 0, int64(0)
		var lastHash []byte
		switch err := tx.QueryRow(ctx,
			`SELECT package_seq, to_seq, package_hash FROM evidence_packages WHERE tenant_id = $1 ORDER BY package_seq DESC LIMIT 1`,
			req.TenantID).Scan(&lastSeq, &lastTo, &lastHash); {
		case errors.Is(err, pgx.ErrNoRows):
			lastHash = ledger.Genesis
		case err != nil:
			return err
		}
		if toSeq <= lastTo {
			return domain.ErrNothingToSeal
		}

		links, err := readLinks(ctx, tx, req.TenantID, toSeq)
		if err != nil {
			return err
		}
		if checked, brk := verifyLinks(links); brk != nil || checked != toSeq {
			return domain.ErrChainBroken
		}
		leaves := make([][]byte, len(links))
		for i, l := range links {
			leaves[i] = l.curr
		}
		root := ledger.MerkleRoot(leaves)
		head := links[len(links)-1]
		packageHash := ledger.PackageHash(lastHash, root, head.curr, toSeq)

		pkg, err = scanPackage(tx.QueryRow(ctx, `
			INSERT INTO evidence_packages (tenant_id, package_id, package_seq, to_seq, leaf_count, merkle_root,
				head_hash, head_decision_id, prev_package_hash, package_hash, label, reason, sealed_by,
				idempotency_key, request_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			RETURNING `+packageColumns,
			req.TenantID, uuid.NewString(), lastSeq+1, toSeq, int64(len(leaves)), root,
			head.curr, head.id, lastHash, packageHash, nullableString(req.Label), nullableString(req.Reason),
			req.PrincipalID, req.IdempotencyKey, req.RequestHash))
		if err != nil {
			return classifyWriteError(err)
		}
		created = true
		return nil
	})
	if err != nil {
		return nil, false, s.unavailable("SealPackage", err, zap.String("tenant_id", req.TenantID))
	}
	return pkg, created, nil
}

// ListPackages is the tenant's packages, newest first.
func (s *PgStore) ListPackages(ctx context.Context, tenantID string) ([]*domain.EvidencePackage, error) {
	var out []*domain.EvidencePackage
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = readPackages(ctx, tx, tenantID)
		return err
	})
	if err != nil {
		return nil, s.unavailable("ListPackages", err)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// GetPackage returns one package of the tenant.
func (s *PgStore) GetPackage(ctx context.Context, tenantID, packageID string) (*domain.EvidencePackage, error) {
	if _, err := uuid.Parse(packageID); err != nil {
		return nil, domain.ErrPackageNotFound
	}
	var pkg *domain.EvidencePackage
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		pkg, err = scanPackage(tx.QueryRow(ctx,
			`SELECT `+packageColumns+` FROM evidence_packages WHERE tenant_id = $1 AND package_id = $2::uuid`,
			tenantID, packageID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPackageNotFound
		}
		return err
	})
	if err != nil {
		return nil, s.unavailable("GetPackage", err, zap.String("package_id", packageID))
	}
	return pkg, nil
}

// VerifyPackage re-computes one package against the chain as stored now.
func (s *PgStore) VerifyPackage(ctx context.Context, tenantID, packageID string) (*domain.PackageCheck, error) {
	if _, err := uuid.Parse(packageID); err != nil {
		return nil, domain.ErrPackageNotFound
	}
	var check domain.PackageCheck
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		p, err := scanPackage(tx.QueryRow(ctx,
			`SELECT `+packageColumns+` FROM evidence_packages WHERE tenant_id = $1 AND package_id = $2::uuid`,
			tenantID, packageID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPackageNotFound
		}
		if err != nil {
			return err
		}
		prev := ledger.Genesis
		if p.PackageSeq > 1 {
			var before []byte
			if err := tx.QueryRow(ctx,
				`SELECT package_hash FROM evidence_packages WHERE tenant_id = $1 AND package_seq = $2`,
				tenantID, p.PackageSeq-1).Scan(&before); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			prev = before
		}
		links, err := readLinks(ctx, tx, tenantID, p.ToSeq)
		if err != nil {
			return err
		}
		leaves := make([][]byte, len(links))
		for i, l := range links {
			leaves[i] = l.curr
		}
		check = checkPackage(p, leaves, prev)
		return nil
	})
	if err != nil {
		return nil, s.unavailable("VerifyPackage", err, zap.String("package_id", packageID))
	}
	return &check, nil
}

// DecisionEvidence is one decision's link (re-computed), the first package
// sealing it, and the events queued for it.
func (s *PgStore) DecisionEvidence(ctx context.Context, tenantID, decisionID string) (*domain.DecisionEvidence, error) {
	ev := &domain.DecisionEvidence{
		DecisionID: decisionID,
		Events:     []domain.OutboxEvent{},
		EventsRetained: "Published events are pruned from the outbox after OUTBOX_RETENTION (default 7 days); " +
			"unpublished events are never pruned.",
	}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var prev, curr []byte
		var canonical string
		var storedAt time.Time
		err := tx.QueryRow(ctx, `SELECT g.chain_seq, g.prev_hash, g.curr_hash, gdl_canonical(g), g.stored_at
			FROM governance_decisions g WHERE g.tenant_id = $1 AND g.decision_id = $2`,
			tenantID, decisionID).Scan(&ev.ChainSeq, &prev, &curr, &canonical, &storedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDecisionNotFound
		}
		if err != nil {
			return err
		}
		ev.DisplayRef = domain.DisplayRef(storedAt, ev.ChainSeq)
		ev.PrevHash = hex.EncodeToString(prev)
		ev.CurrHash = hex.EncodeToString(curr)
		ev.HashValid = ledger.LinkValid(prev, curr, canonical)

		expectPrev := ledger.Genesis
		if ev.ChainSeq > 1 {
			var before []byte
			if err := tx.QueryRow(ctx,
				`SELECT curr_hash FROM governance_decisions WHERE tenant_id = $1 AND chain_seq = $2`,
				tenantID, ev.ChainSeq-1).Scan(&before); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			expectPrev = before
		}
		ev.PrevLinkValid = bytesEqual(prev, expectPrev)

		p, err := scanPackage(tx.QueryRow(ctx, `SELECT `+packageColumns+` FROM evidence_packages
			WHERE tenant_id = $1 AND to_seq >= $2 ORDER BY package_seq LIMIT 1`, tenantID, ev.ChainSeq))
		switch {
		case err == nil:
			ev.Package = p
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		rows, err := tx.Query(ctx, `SELECT event_type, created_at, published_at, attempts, last_error
			FROM outbox WHERE tenant_id = $1 AND aggregate_id = $2 ORDER BY created_at, outbox_id`, tenantID, decisionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.OutboxEvent
			if err := rows.Scan(&e.EventType, &e.CreatedAt, &e.PublishedAt, &e.Attempts, &e.LastError); err != nil {
				return err
			}
			ev.Events = append(ev.Events, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.unavailable("DecisionEvidence", err, zap.String("decision_id", decisionID))
	}
	return ev, nil
}

// UnsealedTenant is a tenant whose chain head is past its last sealed package.
type UnsealedTenant struct {
	TenantID string
	HeadSeq  int64
	SealedTo int64
}

// UnsealedTenants lists every tenant with decisions not yet sealed, for the
// scheduled sealer. It reads across tenants under the app.evidence_sealer
// marker (migration 000017), which admits SELECT only; each seal then runs
// through SealPackage, scoped to its tenant like any request.
func (s *PgStore) UnsealedTenants(ctx context.Context) ([]UnsealedTenant, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.evidence_sealer', 'on', true)`); err != nil {
		return nil, fmt.Errorf("mark sealer transaction: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT h.tenant_id, h.head, COALESCE(p.sealed, 0)
		FROM (SELECT tenant_id, max(chain_seq) AS head FROM governance_decisions GROUP BY tenant_id) h
		LEFT JOIN (SELECT tenant_id, max(to_seq) AS sealed FROM evidence_packages GROUP BY tenant_id) p
		       ON p.tenant_id = h.tenant_id
		WHERE h.head > COALESCE(p.sealed, 0)
		ORDER BY h.tenant_id`)
	if err != nil {
		return nil, s.unavailable("UnsealedTenants", err)
	}
	defer rows.Close()
	var out []UnsealedTenant
	for rows.Next() {
		var u UnsealedTenant
		if err := rows.Scan(&u.TenantID, &u.HeadSeq, &u.SealedTo); err != nil {
			return nil, s.unavailable("UnsealedTenants", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, s.unavailable("UnsealedTenants", err)
	}
	return out, tx.Commit(ctx)
}
