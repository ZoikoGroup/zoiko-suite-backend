package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/ncd"
)

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

// ── NCD-01 intents ──────────────────────────────────────────────────────────

const intentCols = `intent_id::text, version, tenant_id, legal_entity_id, intent_code, display_name, purpose_class,
	domain_owner, sensitivity, urgency, evidence_class, allowed_channels, fallback_allowed, marketing_allowed,
	mandatory, preference_override_allowed, quiet_hours_policy, bulk_allowed, record_requirement, ack_requirement,
	variable_contract, attachment_contract, approved_url_domains, default_expiry_seconds, status,
	created_by_principal_id, created_at, COALESCE(approved_by_principal_id, ''), activated_at, effective_from,
	retired_at, COALESCE(retired_by_principal_id, '')`

func scanIntent(r scannable) (*ncd.Intent, error) {
	var i ncd.Intent
	var purpose, sens, urg, ev string
	var vc, ac []byte
	err := r.Scan(&i.IntentID, &i.Version, &i.TenantID, &i.LegalEntityID, &i.IntentCode, &i.DisplayName, &purpose,
		&i.DomainOwner, &sens, &urg, &ev, &i.AllowedChannels, &i.FallbackAllowed, &i.MarketingAllowed,
		&i.Mandatory, &i.PreferenceOverrideAllowed, &i.QuietHoursPolicy, &i.BulkAllowed, &i.RecordRequirement, &i.AckRequirement,
		&vc, &ac, &i.ApprovedURLDomains, &i.DefaultExpirySeconds, &i.Status,
		&i.CreatedByPrincipalID, &i.CreatedAt, &i.ApprovedByPrincipalID, &i.ActivatedAt, &i.EffectiveFrom,
		&i.RetiredAt, &i.RetiredByPrincipalID)
	if err != nil {
		return nil, nf(err)
	}
	i.PurposeClass, i.Sensitivity, i.Urgency, i.EvidenceClass = ncd.PurposeClass(purpose), ncd.Level(sens), ncd.Level(urg), ncd.Level(ev)
	if err := json.Unmarshal(vc, &i.VariableContract); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(ac, &i.AttachmentContract); err != nil {
		return nil, err
	}
	return &i, nil
}

func (t *ncdTx) InsertIntent(i *ncd.Intent) error {
	i.TenantID = t.tenant
	_, err := t.tx.Exec(t.ctx, `
		INSERT INTO ncd_communication_intents (intent_id, version, tenant_id, legal_entity_id, intent_code, display_name,
			purpose_class, domain_owner, sensitivity, urgency, evidence_class, allowed_channels, fallback_allowed,
			marketing_allowed, mandatory, preference_override_allowed, quiet_hours_policy, bulk_allowed, record_requirement,
			ack_requirement, variable_contract, attachment_contract, approved_url_domains, default_expiry_seconds, status,
			created_by_principal_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
		i.IntentID, i.Version, t.tenant, i.LegalEntityID, i.IntentCode, i.DisplayName, string(i.PurposeClass), i.DomainOwner,
		string(i.Sensitivity), string(i.Urgency), string(i.EvidenceClass), i.AllowedChannels, i.FallbackAllowed,
		i.MarketingAllowed, i.Mandatory, i.PreferenceOverrideAllowed, i.QuietHoursPolicy, i.BulkAllowed, i.RecordRequirement,
		i.AckRequirement, mustJSON(i.VariableContract), mustJSON(i.AttachmentContract), i.ApprovedURLDomains,
		i.DefaultExpirySeconds, i.Status, i.CreatedByPrincipalID, i.CreatedAt)
	return err
}

func (t *ncdTx) GetIntent(id string, version int) (*ncd.Intent, error) {
	if version > 0 {
		return scanIntent(t.tx.QueryRow(t.ctx, `SELECT `+intentCols+` FROM ncd_communication_intents
			WHERE tenant_id = $1 AND intent_id = $2 AND version = $3`, t.tenant, id, version))
	}
	return scanIntent(t.tx.QueryRow(t.ctx, `SELECT `+intentCols+` FROM ncd_communication_intents
		WHERE tenant_id = $1 AND intent_id = $2 ORDER BY version DESC LIMIT 1`, t.tenant, id))
}

func (t *ncdTx) ListIntentVersions(id string) ([]ncd.Intent, error) {
	rows, err := t.tx.Query(t.ctx, `SELECT `+intentCols+` FROM ncd_communication_intents
		WHERE tenant_id = $1 AND intent_id = $2 ORDER BY version`, t.tenant, id)
	if err != nil {
		return nil, nf(err)
	}
	defer rows.Close()
	var out []ncd.Intent
	for rows.Next() {
		i, err := scanIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

func (t *ncdTx) ActivateIntent(id string, version int, approver string, activatedAt, effectiveFrom time.Time) error {
	tag, err := t.tx.Exec(t.ctx, `
		UPDATE ncd_communication_intents SET status = 'ACTIVE', approved_by_principal_id = $4,
		       activated_at = $5, effective_from = $6
		WHERE tenant_id = $1 AND intent_id = $2 AND version = $3 AND status = 'DRAFT'`,
		t.tenant, id, version, approver, activatedAt, effectiveFrom)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ncd.ErrNotFound
	}
	return nil
}

func (t *ncdTx) RetireIntent(id, actor string, at time.Time) (int, error) {
	tag, err := t.tx.Exec(t.ctx, `
		UPDATE ncd_communication_intents SET status = 'RETIRED', retired_at = $3, retired_by_principal_id = $4
		WHERE tenant_id = $1 AND intent_id = $2 AND status IN ('DRAFT','ACTIVE')`, t.tenant, id, at, actor)
	return int(tag.RowsAffected()), err
}

func (t *ncdTx) EffectiveIntent(id string, txTime, knownAt time.Time) (*ncd.Intent, error) {
	return scanIntent(t.tx.QueryRow(t.ctx, `SELECT `+intentCols+` FROM ncd_communication_intents
		WHERE tenant_id = $1 AND intent_id = $2
		  AND activated_at IS NOT NULL AND activated_at <= $4 AND effective_from <= $3
		  AND (retired_at IS NULL OR retired_at > $4)
		ORDER BY effective_from DESC, version DESC LIMIT 1`, t.tenant, id, txTime, knownAt))
}

// ── NCD-01 templates ────────────────────────────────────────────────────────

const templateCols = `template_version_id::text, template_id::text, version, tenant_id, legal_entity_id, intent_id::text,
	intent_version, channel, locale, compatible_locales, subject, body, content_hash, schema_hash, status,
	created_by_principal_id, created_at, validated_at, validation_report, COALESCE(approved_by_principal_id, ''),
	approved_at, COALESCE(published_by_principal_id, ''), published_at, effective_from, retired_at`

func scanTemplate(r scannable) (*ncd.TemplateVersion, error) {
	var tv ncd.TemplateVersion
	var report []byte
	err := r.Scan(&tv.TemplateVersionID, &tv.TemplateID, &tv.Version, &tv.TenantID, &tv.LegalEntityID, &tv.IntentID,
		&tv.IntentVersion, &tv.Channel, &tv.Locale, &tv.CompatibleLocales, &tv.Subject, &tv.Body, &tv.ContentHash, &tv.SchemaHash,
		&tv.Status, &tv.CreatedByPrincipalID, &tv.CreatedAt, &tv.ValidatedAt, &report, &tv.ApprovedByPrincipalID,
		&tv.ApprovedAt, &tv.PublishedByPrincipalID, &tv.PublishedAt, &tv.EffectiveFrom, &tv.RetiredAt)
	if err != nil {
		return nil, nf(err)
	}
	if len(report) > 0 && string(report) != "null" {
		var vr ncd.ValidationReport
		if err := json.Unmarshal(report, &vr); err != nil {
			return nil, err
		}
		tv.ValidationReport = &vr
	}
	return &tv, nil
}

func (t *ncdTx) scanTemplates(q string, args ...any) ([]ncd.TemplateVersion, error) {
	rows, err := t.tx.Query(t.ctx, q, args...)
	if err != nil {
		return nil, nf(err)
	}
	defer rows.Close()
	var out []ncd.TemplateVersion
	for rows.Next() {
		tv, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *tv)
	}
	return out, rows.Err()
}

func (t *ncdTx) NextTemplateSlot(intentID, channel, locale string) (string, int, error) {
	var tid string
	var maxV int
	err := t.tx.QueryRow(t.ctx, `
		SELECT template_id::text, max(version) FROM ncd_templates
		WHERE tenant_id = $1 AND intent_id = $2 AND channel = $3 AND locale = $4
		GROUP BY template_id LIMIT 1`, t.tenant, intentID, channel, locale).Scan(&tid, &maxV)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.NewString(), 1, nil
	}
	if err != nil {
		return "", 0, nf(err)
	}
	return tid, maxV + 1, nil
}

func (t *ncdTx) InsertTemplate(tv *ncd.TemplateVersion) error {
	tv.TenantID = t.tenant
	_, err := t.tx.Exec(t.ctx, `
		INSERT INTO ncd_templates (template_version_id, template_id, version, tenant_id, legal_entity_id, intent_id,
			intent_version, channel, locale, compatible_locales, subject, body, content_hash, schema_hash, status,
			created_by_principal_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		tv.TemplateVersionID, tv.TemplateID, tv.Version, t.tenant, tv.LegalEntityID, tv.IntentID, tv.IntentVersion,
		tv.Channel, tv.Locale, tv.CompatibleLocales, tv.Subject, tv.Body, tv.ContentHash, tv.SchemaHash, tv.Status,
		tv.CreatedByPrincipalID, tv.CreatedAt)
	return err
}

func (t *ncdTx) GetTemplate(id string) (*ncd.TemplateVersion, error) {
	return scanTemplate(t.tx.QueryRow(t.ctx, `SELECT `+templateCols+` FROM ncd_templates
		WHERE tenant_id = $1 AND template_version_id = $2`, t.tenant, id))
}

func (t *ncdTx) UpdateTemplate(tv *ncd.TemplateVersion) error {
	var report any
	if tv.ValidationReport != nil {
		report = mustJSON(tv.ValidationReport)
	}
	_, err := t.tx.Exec(t.ctx, `
		UPDATE ncd_templates SET status = $3, validated_at = $4, validation_report = $5,
		       approved_by_principal_id = $6, approved_at = $7, published_by_principal_id = $8,
		       published_at = $9, effective_from = $10, retired_at = $11
		WHERE tenant_id = $1 AND template_version_id = $2`,
		t.tenant, tv.TemplateVersionID, tv.Status, tv.ValidatedAt, report, nullStr(tv.ApprovedByPrincipalID), tv.ApprovedAt,
		nullStr(tv.PublishedByPrincipalID), tv.PublishedAt, tv.EffectiveFrom, tv.RetiredAt)
	return err
}

func (t *ncdTx) ListTemplates(intentID string) ([]ncd.TemplateVersion, error) {
	return t.scanTemplates(`SELECT `+templateCols+` FROM ncd_templates
		WHERE tenant_id = $1 AND intent_id = $2 ORDER BY channel, locale, version`, t.tenant, intentID)
}

func (t *ncdTx) EffectiveTemplates(intentID string, txTime, knownAt time.Time) ([]ncd.TemplateVersion, error) {
	return t.scanTemplates(`SELECT DISTINCT ON (channel, locale) `+templateCols+` FROM ncd_templates
		WHERE tenant_id = $1 AND intent_id = $2
		  AND published_at IS NOT NULL AND published_at <= $4 AND effective_from <= $3
		  AND (retired_at IS NULL OR retired_at > $4)
		ORDER BY channel, locale, effective_from DESC, version DESC`, t.tenant, intentID, txTime, knownAt)
}

// ── NCD-02 preferences ──────────────────────────────────────────────────────

func (t *ncdTx) GetPreference(principalID string) (*ncd.Preference, error) {
	var p ncd.Preference
	err := t.tx.QueryRow(t.ctx, `
		SELECT tenant_id, principal_id, muted_channels, channel_order,
		       COALESCE(to_char(quiet_hours_start, 'HH24:MI'), ''), COALESCE(to_char(quiet_hours_end, 'HH24:MI'), ''),
		       COALESCE(time_zone, ''), COALESCE(locale, ''), version, updated_at, updated_by_principal_id
		FROM ncd_preferences WHERE tenant_id = $1 AND principal_id = $2`, t.tenant, principalID).Scan(
		&p.TenantID, &p.PrincipalID, &p.MutedChannels, &p.ChannelOrder, &p.QuietHoursStart, &p.QuietHoursEnd,
		&p.TimeZone, &p.Locale, &p.Version, &p.UpdatedAt, &p.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (t *ncdTx) SavePreference(p *ncd.Preference, expectedVersion int) error {
	var tag interface{ RowsAffected() int64 }
	var err error
	if expectedVersion == 0 {
		tag, err = t.tx.Exec(t.ctx, `
			INSERT INTO ncd_preferences (tenant_id, principal_id, muted_channels, channel_order, quiet_hours_start,
				quiet_hours_end, time_zone, locale, version, updated_at, updated_by_principal_id)
			VALUES ($1,$2,$3,$4,NULLIF($5,'')::time,NULLIF($6,'')::time,NULLIF($7,''),NULLIF($8,''),1,$9,$10)
			ON CONFLICT (tenant_id, principal_id) DO NOTHING`,
			t.tenant, p.PrincipalID, p.MutedChannels, p.ChannelOrder, p.QuietHoursStart, p.QuietHoursEnd,
			p.TimeZone, p.Locale, p.UpdatedAt, p.UpdatedBy)
		p.Version = 1
	} else {
		tag, err = t.tx.Exec(t.ctx, `
			UPDATE ncd_preferences SET muted_channels = $3, channel_order = $4,
			       quiet_hours_start = NULLIF($5,'')::time, quiet_hours_end = NULLIF($6,'')::time,
			       time_zone = NULLIF($7,''), locale = NULLIF($8,''), version = version + 1,
			       updated_at = $9, updated_by_principal_id = $10
			WHERE tenant_id = $1 AND principal_id = $2 AND version = $11`,
			t.tenant, p.PrincipalID, p.MutedChannels, p.ChannelOrder, p.QuietHoursStart, p.QuietHoursEnd,
			p.TimeZone, p.Locale, p.UpdatedAt, p.UpdatedBy, expectedVersion)
		p.Version = expectedVersion + 1
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ncd.ErrStaleVersion
	}
	_, err = t.tx.Exec(t.ctx, `
		INSERT INTO ncd_preference_changes (change_id, tenant_id, principal_id, version, snapshot, changed_by_principal_id, changed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, uuid.NewString(), t.tenant, p.PrincipalID, p.Version, mustJSON(p), p.UpdatedBy, p.UpdatedAt)
	return err
}

// ── NCD-02 suppressions ─────────────────────────────────────────────────────

const suppCols = `suppression_id::text, tenant_id, COALESCE(subject_principal_id, ''), COALESCE(endpoint_hash, ''),
	COALESCE(endpoint_masked, ''), channel_scope, purpose_scope, reason, source, source_evidence_ref, effective_from,
	effective_until, created_by_principal_id, created_at, lifted_at, COALESCE(lifted_by_principal_id, ''),
	COALESCE(lift_evidence_ref, ''), COALESCE(lift_approved_by_principal_id, ''), false`

// legacySuppCols projects the pre-NCD email_suppressions list into the
// canonical shape. The endpoint hash is computed in SQL with exactly the
// formula of ncd.EndpointHash, so the two lists match on one key (NP-45).
// A legacy row has no effective-from: it is in force from the moment it
// exists, so it is projected as effective since the epoch — clock skew between
// the service and the database must never open a window in which a recorded
// bounce is ignored. Reason mapping follows §5.4: an unsubscribe or a complaint scopes to
// marketing — it cannot silence a security notice (§5.3).
const legacySuppCols = `e.suppression_id::text, e.tenant_id, '',
	encode(sha256(convert_to('endpoint' || chr(31) || 'EMAIL' || chr(31) || lower(btrim(e.recipient_email)), 'UTF8')), 'hex'),
	'legacy', 'EMAIL',
	CASE WHEN e.reason IN ('UNSUBSCRIBE','COMPLAINT') THEN 'MARKETING_PROMOTIONAL'
	     WHEN e.source_stream = 'MARKETING' THEN 'MARKETING_PROMOTIONAL'
	     WHEN e.source_stream = 'TRANSACTIONAL' THEN 'TRANSACTIONAL_RELATIONSHIP'
	     WHEN e.source_stream = 'OPERATIONAL' THEN 'OPERATIONAL_WORKFLOW'
	     WHEN e.source_stream = 'CRITICAL' THEN 'SECURITY_CRITICAL'
	     ELSE 'ALL' END,
	CASE e.reason WHEN 'HARD_BOUNCE' THEN 'HARD_BOUNCE' WHEN 'COMPLAINT' THEN 'COMPLAINT_ABUSE'
	     WHEN 'UNSUBSCRIBE' THEN 'MARKETING_OPTOUT' ELSE 'SECURITY_HOLD' END,
	CASE WHEN e.provider_name IS NOT NULL THEN 'PROVIDER_EVENT' ELSE 'OPERATOR' END,
	'legacy:email_suppressions/' || e.suppression_id::text, 'epoch'::timestamptz, NULL::timestamptz, 'legacy', e.created_at,
	e.lifted_at, COALESCE(e.lifted_by_principal_id, ''), COALESCE(e.lift_evidence_ref, ''),
	COALESCE(e.lift_approved_by_principal_id, ''), true`

func scanSupp(r scannable) (*ncd.Suppression, error) {
	var s ncd.Suppression
	err := r.Scan(&s.SuppressionID, &s.TenantID, &s.SubjectPrincipalID, &s.EndpointHash, &s.EndpointMasked,
		&s.ChannelScope, &s.PurposeScope, &s.Reason, &s.Source, &s.SourceEvidenceRef, &s.EffectiveFrom,
		&s.EffectiveUntil, &s.CreatedByPrincipal, &s.CreatedAt, &s.LiftedAt, &s.LiftedByPrincipal,
		&s.LiftEvidenceRef, &s.LiftApprovedBy, &s.Legacy)
	if err != nil {
		return nil, nf(err)
	}
	return &s, nil
}

func (t *ncdTx) scanSupps(q string, args ...any) ([]ncd.Suppression, error) {
	rows, err := t.tx.Query(t.ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ncd.Suppression
	for rows.Next() {
		s, err := scanSupp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func (t *ncdTx) InsertSuppression(s *ncd.Suppression) (bool, error) {
	s.TenantID = t.tenant
	tag, err := t.tx.Exec(t.ctx, `
		INSERT INTO ncd_suppressions (suppression_id, tenant_id, subject_principal_id, endpoint_hash, endpoint_masked,
			channel_scope, purpose_scope, reason, source, source_evidence_ref, effective_from, effective_until,
			created_by_principal_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT DO NOTHING`,
		s.SuppressionID, t.tenant, nullStr(s.SubjectPrincipalID), nullStr(s.EndpointHash), nullStr(s.EndpointMasked),
		s.ChannelScope, s.PurposeScope, s.Reason, s.Source, s.SourceEvidenceRef, s.EffectiveFrom, s.EffectiveUntil,
		s.CreatedByPrincipal, s.CreatedAt)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	existing, err := scanSupp(t.tx.QueryRow(t.ctx, `SELECT `+suppCols+` FROM ncd_suppressions
		WHERE tenant_id = $1 AND COALESCE(endpoint_hash, '') = $2 AND COALESCE(subject_principal_id, '') = $3
		  AND channel_scope = $4 AND purpose_scope = $5 AND reason = $6 AND source_evidence_ref = $7`,
		t.tenant, s.EndpointHash, s.SubjectPrincipalID, s.ChannelScope, s.PurposeScope, s.Reason, s.SourceEvidenceRef))
	if err != nil {
		return false, err
	}
	*s = *existing
	return false, nil
}

// GetSuppression reads a canonical suppression or, failing that, a legacy
// email_suppressions row projected into the canonical shape. The send gate
// honours both lists, so both are lifted the same governed way (000022).
func (t *ncdTx) GetSuppression(id string) (*ncd.Suppression, error) {
	s, err := scanSupp(t.tx.QueryRow(t.ctx, `SELECT `+suppCols+` FROM ncd_suppressions
		WHERE tenant_id = $1 AND suppression_id = $2`, t.tenant, id))
	if !errors.Is(err, ncd.ErrNotFound) {
		return s, err
	}
	return scanSupp(t.tx.QueryRow(t.ctx, `SELECT `+legacySuppCols+` FROM email_suppressions e
		WHERE e.tenant_id = $1 AND e.suppression_id::text = $2`, t.tenant, id))
}

// LiftSuppression lifts a canonical row or, failing that, a legacy one. The
// database enforces the same evidence and second-principal rules on both.
func (t *ncdTx) LiftSuppression(id, liftedBy, evidenceRef, approvedBy string, at time.Time) error {
	for _, q := range []string{`
		UPDATE ncd_suppressions SET lifted_at = $3, lifted_by_principal_id = $4, lift_evidence_ref = $5,
		       lift_approved_by_principal_id = $6
		WHERE tenant_id = $1 AND suppression_id = $2 AND lifted_at IS NULL`, `
		UPDATE email_suppressions SET lifted_at = $3, lifted_by_principal_id = $4, lift_evidence_ref = $5,
		       lift_approved_by_principal_id = $6
		WHERE tenant_id = $1 AND suppression_id::text = $2 AND lifted_at IS NULL`} {
		tag, err := t.tx.Exec(t.ctx, q, t.tenant, id, at, liftedBy, evidenceRef, nullStr(approvedBy))
		if err != nil {
			return nf(err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
	}
	return ncd.ErrNotFound
}

func (t *ncdTx) ListSuppressions(principalID, endpointHash string, activeOnly bool, limit int) ([]ncd.Suppression, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	return t.scanSupps(`
		SELECT * FROM (
			SELECT `+suppCols+` FROM ncd_suppressions
			WHERE tenant_id = $1 AND ($2 = '' OR subject_principal_id = $2) AND ($3 = '' OR endpoint_hash = $3)
			  AND (NOT $4 OR lifted_at IS NULL)
			UNION ALL
			SELECT `+legacySuppCols+` FROM email_suppressions e
			WHERE e.tenant_id = $1 AND $2 = '' AND (NOT $4 OR e.lifted_at IS NULL)
			  AND ($3 = '' OR encode(sha256(convert_to('endpoint' || chr(31) || 'EMAIL' || chr(31) || lower(btrim(e.recipient_email)), 'UTF8')), 'hex') = $3)
		) s ORDER BY 14 DESC LIMIT $5`, t.tenant, principalID, endpointHash, activeOnly, limit)
}

func (t *ncdTx) ActiveSuppressions(principalID string, endpointHashes, _ []string, now time.Time) ([]ncd.Suppression, error) {
	if endpointHashes == nil {
		endpointHashes = []string{}
	}
	return t.scanSupps(`
		SELECT `+suppCols+` FROM ncd_suppressions
		WHERE tenant_id = $1 AND lifted_at IS NULL AND effective_from <= $4
		  AND (effective_until IS NULL OR effective_until > $4)
		  AND (subject_principal_id = $2 OR endpoint_hash = ANY($3))
		UNION ALL
		SELECT `+legacySuppCols+` FROM email_suppressions e
		WHERE e.tenant_id = $1 AND e.lifted_at IS NULL
		  AND encode(sha256(convert_to('endpoint' || chr(31) || 'EMAIL' || chr(31) || lower(btrim(e.recipient_email)), 'UTF8')), 'hex') = ANY($3)`,
		t.tenant, principalID, endpointHashes, now)
}

func (t *ncdTx) CountSoftBounces(endpointHash string, since time.Time) (int, error) {
	var n int
	err := t.tx.QueryRow(t.ctx, `SELECT count(*) FROM ncd_suppressions
		WHERE tenant_id = $1 AND endpoint_hash = $2 AND reason = 'TEMP_SOFT_BOUNCE' AND created_at >= $3`,
		t.tenant, endpointHash, since).Scan(&n)
	return n, err
}

// ── NCD-02 plans and decisions ──────────────────────────────────────────────

func (t *ncdTx) InsertPlan(p *ncd.RecipientPlan) error {
	p.TenantID = t.tenant
	eps, err := ncd.MarshalEndpointsForStorage(p.Endpoints)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(t.ctx, `
		INSERT INTO ncd_recipient_plans (plan_id, tenant_id, legal_entity_id, intent_id, intent_version, recipient_principal_id,
			endpoints, locale, time_zone, time_zone_source, decision_evidence, created_by_principal_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),NULLIF($10,''),$11,$12,$13)`,
		p.PlanID, t.tenant, p.LegalEntityID, p.IntentID, p.IntentVersion, p.RecipientPrincipalID, eps, p.Locale,
		p.TimeZone, p.TimeZoneSource, mustJSON(p.DecisionEvidence), p.CreatedByPrincipalID, p.CreatedAt)
	return err
}

func (t *ncdTx) GetPlan(id string) (*ncd.RecipientPlan, error) {
	var p ncd.RecipientPlan
	var eps, ev []byte
	err := t.tx.QueryRow(t.ctx, `
		SELECT plan_id::text, tenant_id, legal_entity_id, intent_id::text, intent_version, recipient_principal_id, endpoints,
		       COALESCE(locale, ''), COALESCE(time_zone, ''), COALESCE(time_zone_source, ''), decision_evidence,
		       created_by_principal_id, created_at
		FROM ncd_recipient_plans WHERE tenant_id = $1 AND plan_id = $2`, t.tenant, id).Scan(
		&p.PlanID, &p.TenantID, &p.LegalEntityID, &p.IntentID, &p.IntentVersion, &p.RecipientPrincipalID, &eps,
		&p.Locale, &p.TimeZone, &p.TimeZoneSource, &ev, &p.CreatedByPrincipalID, &p.CreatedAt)
	if err != nil {
		return nil, nf(err)
	}
	if p.Endpoints, err = ncd.UnmarshalEndpointsFromStorage(eps); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(ev, &p.DecisionEvidence)
	return &p, nil
}

const decisionCols = `decision_id::text, tenant_id, plan_id::text, intent_id::text, intent_version,
	COALESCE(communication_id::text, ''), outcome, routes, restrictions, reason_codes, not_before,
	evidence_requirement, fallback_rules, inputs, decided_by_principal_id, decided_at`

func scanDecision(r scannable) (*ncd.ChannelDecision, error) {
	var d ncd.ChannelDecision
	var routes, restr, fb, inputs []byte
	var codes []string
	var ev string
	err := r.Scan(&d.DecisionID, &d.TenantID, &d.PlanID, &d.IntentID, &d.IntentVersion, &d.CommunicationID, &d.Outcome,
		&routes, &restr, &codes, &d.NotBefore, &ev, &fb, &inputs, &d.DecidedBy, &d.DecidedAt)
	if err != nil {
		return nil, nf(err)
	}
	d.EvidenceRequirement = ncd.Level(ev)
	if d.Routes, err = ncd.UnmarshalRoutesFromStorage(routes); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(restr, &d.Restrictions)
	_ = json.Unmarshal(fb, &d.FallbackRules)
	_ = json.Unmarshal(inputs, &d.Inputs)
	for _, c := range codes {
		d.ReasonCodes = append(d.ReasonCodes, ncd.ReasonCode(c))
	}
	return &d, nil
}

func (t *ncdTx) InsertDecision(d *ncd.ChannelDecision) error {
	routes, err := ncd.MarshalRoutesForStorage(d.Routes)
	if err != nil {
		return err
	}
	codes := make([]string, len(d.ReasonCodes))
	for i, c := range d.ReasonCodes {
		codes[i] = string(c)
	}
	_, err = t.tx.Exec(t.ctx, `
		INSERT INTO ncd_channel_decisions (decision_id, tenant_id, plan_id, intent_id, intent_version, communication_id,
			outcome, routes, restrictions, reason_codes, not_before, evidence_requirement, fallback_rules, inputs,
			decided_by_principal_id, decided_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		d.DecisionID, t.tenant, d.PlanID, d.IntentID, d.IntentVersion, nullStr(d.CommunicationID), d.Outcome, routes,
		mustJSON(d.Restrictions), codes, d.NotBefore, string(d.EvidenceRequirement), mustJSON(d.FallbackRules),
		mustJSON(d.Inputs), d.DecidedBy, d.DecidedAt)
	return err
}

func (t *ncdTx) GetDecision(id string) (*ncd.ChannelDecision, error) {
	return scanDecision(t.tx.QueryRow(t.ctx, `SELECT `+decisionCols+` FROM ncd_channel_decisions
		WHERE tenant_id = $1 AND decision_id = $2`, t.tenant, id))
}

func (t *ncdTx) ListDecisions(communicationID string) ([]ncd.ChannelDecision, error) {
	rows, err := t.tx.Query(t.ctx, `SELECT `+decisionCols+` FROM ncd_channel_decisions
		WHERE tenant_id = $1 AND communication_id = $2 ORDER BY decided_at`, t.tenant, communicationID)
	if err != nil {
		return nil, nf(err)
	}
	defer rows.Close()
	var out []ncd.ChannelDecision
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

func (t *ncdTx) GetStreamControl(stream string) (string, string, error) {
	var state, reason string
	err := t.tx.QueryRow(t.ctx, `SELECT state, reason FROM ncd_stream_controls WHERE tenant_id = $1 AND stream = $2`,
		t.tenant, stream).Scan(&state, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "ACTIVE", "", nil
	}
	return state, reason, err
}

func (t *ncdTx) SetStreamControl(stream, state, reason, actor string, automatic bool) error {
	_, err := t.tx.Exec(t.ctx, `
		INSERT INTO ncd_stream_controls (tenant_id, stream, state, reason, automatic, changed_by_principal_id, changed_at)
		VALUES ($1,$2,$3,$4,$5,$6,now())
		ON CONFLICT (tenant_id, stream) DO UPDATE SET state = EXCLUDED.state, reason = EXCLUDED.reason,
		       automatic = EXCLUDED.automatic, changed_by_principal_id = EXCLUDED.changed_by_principal_id, changed_at = now()`,
		t.tenant, stream, state, reason, automatic, actor)
	return err
}

// ── communications ──────────────────────────────────────────────────────────

const commCols = `communication_id::text, tenant_id, legal_entity_id, intent_id::text, COALESCE(intent_version, 0),
	COALESCE(purpose_class, ''), idempotency_key, source_event_id, COALESCE(source_event_type, ''), COALESCE(workflow_id, ''),
	recipient_principal_id, COALESCE(recipient_tenant_id, ''), free_text_endpoint, locale, variables, attachments,
	privacy_permission, marketing_permission, COALESCE(pdc_decision_ref, ''), residency_regions, lifecycle_state,
	COALESCE(blocked_reason_code, ''), COALESCE(blocked_detail, ''), COALESCE(recipient_plan_id::text, ''),
	COALESCE(channel_decision_id::text, ''), not_before, expires_at, priority,
	COALESCE(supersedes_communication_id::text, ''), COALESCE(correction_reason, ''), COALESCE(bulk_id::text, ''),
	created_by_principal_id, created_at, prepared_at, dispatched_at, concluded_at`

func scanComm(r scannable) (*ncd.Communication, error) {
	var c ncd.Communication
	var purpose, code string
	var free, vars, atts, priv, mkt []byte
	err := r.Scan(&c.CommunicationID, &c.TenantID, &c.LegalEntityID, &c.IntentID, &c.IntentVersion, &purpose,
		&c.IdempotencyKey, &c.SourceEventID, &c.SourceEventType, &c.WorkflowID, &c.RecipientPrincipalID, &c.RecipientTenantID,
		&free, &c.Locale, &vars, &atts, &priv, &mkt, &c.PDCDecisionRef, &c.ResidencyRegions, &c.LifecycleState,
		&code, &c.BlockedDetail, &c.RecipientPlanID, &c.ChannelDecisionID, &c.NotBefore, &c.ExpiresAt, &c.Priority,
		&c.SupersedesCommunicationID, &c.CorrectionReason, &c.BulkID, &c.CreatedByPrincipalID, &c.CreatedAt,
		&c.PreparedAt, &c.DispatchedAt, &c.ConcludedAt)
	if err != nil {
		return nil, nf(err)
	}
	c.PurposeClass, c.BlockedReasonCode = ncd.PurposeClass(purpose), ncd.ReasonCode(code)
	if len(free) > 0 && string(free) != "null" {
		var f ncd.FreeTextEndpoint
		if err := json.Unmarshal(free, &f); err == nil {
			c.FreeTextEndpoint = &f
		}
	}
	_ = json.Unmarshal(vars, &c.Variables)
	_ = json.Unmarshal(atts, &c.Attachments)
	_ = json.Unmarshal(priv, &c.PrivacyPermission)
	_ = json.Unmarshal(mkt, &c.MarketingPermission)
	if c.Attachments == nil {
		c.Attachments = []ncd.Attachment{}
	}
	return &c, nil
}

func (t *ncdTx) InsertCommunication(c *ncd.Communication) (bool, error) {
	c.TenantID = t.tenant
	var free any
	if c.FreeTextEndpoint != nil {
		free = mustJSON(c.FreeTextEndpoint)
	}
	tag, err := t.tx.Exec(t.ctx, `
		INSERT INTO ncd_communications (communication_id, tenant_id, legal_entity_id, intent_id, idempotency_key,
			source_event_id, source_event_type, workflow_id, recipient_principal_id, recipient_tenant_id, free_text_endpoint,
			locale, variables, attachments, privacy_permission, marketing_permission, pdc_decision_ref, residency_regions,
			lifecycle_state, not_before, expires_at, priority, supersedes_communication_id, correction_reason, bulk_id,
			created_by_principal_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''),$9,NULLIF($10,''),$11,$12,$13,$14,$15,$16,NULLIF($17,''),$18,
			$19,$20,$21,$22,$23,NULLIF($24,''),$25,$26,$27)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
		c.CommunicationID, t.tenant, c.LegalEntityID, c.IntentID, c.IdempotencyKey, c.SourceEventID, c.SourceEventType,
		c.WorkflowID, c.RecipientPrincipalID, c.RecipientTenantID, free, c.Locale, mustJSON(c.Variables),
		mustJSON(c.Attachments), mustJSON(c.PrivacyPermission), mustJSON(c.MarketingPermission), c.PDCDecisionRef,
		c.ResidencyRegions, c.LifecycleState, c.NotBefore, c.ExpiresAt, c.Priority, nullStr(c.SupersedesCommunicationID),
		c.CorrectionReason, nullStr(c.BulkID), c.CreatedByPrincipalID, c.CreatedAt)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	existing, err := scanComm(t.tx.QueryRow(t.ctx, `SELECT `+commCols+` FROM ncd_communications
		WHERE tenant_id = $1 AND idempotency_key = $2`, t.tenant, c.IdempotencyKey))
	if err != nil {
		return false, err
	}
	*c = *existing
	return false, nil
}

func (t *ncdTx) GetCommunication(id string, forUpdate bool) (*ncd.Communication, error) {
	q := `SELECT ` + commCols + ` FROM ncd_communications WHERE tenant_id = $1 AND communication_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	return scanComm(t.tx.QueryRow(t.ctx, q, t.tenant, id))
}

func (t *ncdTx) UpdateCommunication(c *ncd.Communication) error {
	_, err := t.tx.Exec(t.ctx, `
		UPDATE ncd_communications SET intent_version = NULLIF($3, 0), purpose_class = NULLIF($4, ''),
		       lifecycle_state = $5, blocked_reason_code = NULLIF($6, ''), blocked_detail = NULLIF($7, ''),
		       recipient_plan_id = $8, channel_decision_id = $9, not_before = $10, expires_at = $11,
		       prepared_at = $12, dispatched_at = $13, concluded_at = $14
		WHERE tenant_id = $1 AND communication_id = $2`,
		t.tenant, c.CommunicationID, c.IntentVersion, string(c.PurposeClass), c.LifecycleState, string(c.BlockedReasonCode),
		c.BlockedDetail, nullStr(c.RecipientPlanID), nullStr(c.ChannelDecisionID), c.NotBefore, c.ExpiresAt,
		c.PreparedAt, c.DispatchedAt, c.ConcludedAt)
	return err
}

func (t *ncdTx) ListCommunications(recipient string, limit int) ([]ncd.Communication, error) {
	rows, err := t.tx.Query(t.ctx, `SELECT `+commCols+` FROM ncd_communications
		WHERE tenant_id = $1 AND ($2 = '' OR recipient_principal_id = $2) ORDER BY created_at DESC LIMIT $3`,
		t.tenant, recipient, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ncd.Communication
	for rows.Next() {
		c, err := scanComm(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (t *ncdTx) InsertRender(r *ncd.RenderedContent) error {
	_, err := t.tx.Exec(t.ctx, `
		INSERT INTO ncd_rendered_content (render_id, tenant_id, communication_id, channel, template_version_id,
			template_version, locale, locale_fallback_from, subject, body, subject_hash, body_hash, content_hash,
			variable_hashes, attachment_manifest, rendered_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10,$11,$12,$13,$14,$15,$16)`,
		r.RenderID, t.tenant, r.CommunicationID, r.Channel, r.TemplateVersionID, r.TemplateVersion, r.Locale,
		r.LocaleFallbackFrom, r.Subject, r.Body, r.SubjectHash, r.BodyHash, r.ContentHash, mustJSON(r.VariableHashes),
		mustJSON(r.AttachmentManifest), r.RenderedAt)
	return err
}

func (t *ncdTx) ListRenders(communicationID string) ([]ncd.RenderedContent, error) {
	rows, err := t.tx.Query(t.ctx, `
		SELECT render_id::text, communication_id::text, channel, template_version_id::text, template_version, locale,
		       COALESCE(locale_fallback_from, ''), subject, body, subject_hash, body_hash, content_hash,
		       variable_hashes, attachment_manifest, rendered_at
		FROM ncd_rendered_content WHERE tenant_id = $1 AND communication_id = $2 ORDER BY channel`, t.tenant, communicationID)
	if err != nil {
		return nil, nf(err)
	}
	defer rows.Close()
	var out []ncd.RenderedContent
	for rows.Next() {
		var r ncd.RenderedContent
		var vh, am []byte
		if err := rows.Scan(&r.RenderID, &r.CommunicationID, &r.Channel, &r.TemplateVersionID, &r.TemplateVersion,
			&r.Locale, &r.LocaleFallbackFrom, &r.Subject, &r.Body, &r.SubjectHash, &r.BodyHash, &r.ContentHash,
			&vh, &am, &r.RenderedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(vh, &r.VariableHashes)
		_ = json.Unmarshal(am, &r.AttachmentManifest)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── NCD-03 jobs ─────────────────────────────────────────────────────────────

const jobCols = `job_id::text, tenant_id, communication_id::text, origin, COALESCE(resend_reason, ''), routes,
	route_index, attempts_on_route, max_attempts_per_route, state, stream, priority, next_run_at, not_before,
	expires_at, leased_until, COALESCE(last_deferral_reason, ''), COALESCE(exception_reason, ''),
	created_by_principal_id, created_at, concluded_at`

func scanJob(r scannable) (*ncd.DeliveryJob, error) {
	var j ncd.DeliveryJob
	var routes []byte
	err := r.Scan(&j.JobID, &j.TenantID, &j.CommunicationID, &j.Origin, &j.ResendReason, &routes, &j.RouteIndex,
		&j.AttemptsOnRoute, &j.MaxAttemptsPerRoute, &j.State, &j.Stream, &j.Priority, &j.NextRunAt, &j.NotBefore,
		&j.ExpiresAt, &j.LeasedUntil, &j.LastDeferralReason, &j.ExceptionReason, &j.CreatedBy, &j.CreatedAt, &j.ConcludedAt)
	if err != nil {
		return nil, nf(err)
	}
	if j.Routes, err = ncd.UnmarshalRoutesFromStorage(routes); err != nil {
		return nil, err
	}
	return &j, nil
}

func (t *ncdTx) InsertJob(j *ncd.DeliveryJob) error {
	j.TenantID = t.tenant
	routes, err := ncd.MarshalRoutesForStorage(j.Routes)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(t.ctx, `
		INSERT INTO ncd_delivery_jobs (job_id, tenant_id, communication_id, origin, resend_reason, routes, route_index,
			attempts_on_route, max_attempts_per_route, state, stream, priority, next_run_at, not_before, expires_at,
			created_by_principal_id, created_at)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		j.JobID, t.tenant, j.CommunicationID, j.Origin, j.ResendReason, routes, j.RouteIndex, j.AttemptsOnRoute,
		j.MaxAttemptsPerRoute, j.State, j.Stream, j.Priority, j.NextRunAt, j.NotBefore, j.ExpiresAt, j.CreatedBy, j.CreatedAt)
	return err
}

func (t *ncdTx) GetJob(id string) (*ncd.DeliveryJob, error) {
	return scanJob(t.tx.QueryRow(t.ctx, `SELECT `+jobCols+` FROM ncd_delivery_jobs WHERE tenant_id = $1 AND job_id = $2`, t.tenant, id))
}

func (t *ncdTx) ClaimJob(id string, now time.Time, lease time.Duration) (*ncd.DeliveryJob, error) {
	j, err := scanJob(t.tx.QueryRow(t.ctx, `
		UPDATE ncd_delivery_jobs SET leased_until = $3::timestamptz + make_interval(secs => $4)
		WHERE tenant_id = $1 AND job_id = $2 AND state IN ('QUEUED','AWAITING_EVIDENCE') AND next_run_at <= $3
		  AND (leased_until IS NULL OR leased_until < $3)
		RETURNING `+jobCols, t.tenant, id, now, lease.Seconds()))
	if errors.Is(err, ncd.ErrNotFound) {
		return nil, nil
	}
	return j, err
}

func (t *ncdTx) UpdateJob(j *ncd.DeliveryJob) error {
	routes, err := ncd.MarshalRoutesForStorage(j.Routes)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(t.ctx, `
		UPDATE ncd_delivery_jobs SET routes = $3, route_index = $4, attempts_on_route = $5, state = $6,
		       next_run_at = $7, not_before = $8, leased_until = $9, last_deferral_reason = NULLIF($10, ''),
		       exception_reason = NULLIF($11, ''), concluded_at = $12, updated_at = now()
		WHERE tenant_id = $1 AND job_id = $2`,
		t.tenant, j.JobID, routes, j.RouteIndex, j.AttemptsOnRoute, j.State, j.NextRunAt, j.NotBefore, j.LeasedUntil,
		j.LastDeferralReason, j.ExceptionReason, j.ConcludedAt)
	return err
}

func (t *ncdTx) ListJobs(communicationID string) ([]ncd.DeliveryJob, error) {
	rows, err := t.tx.Query(t.ctx, `SELECT `+jobCols+` FROM ncd_delivery_jobs
		WHERE tenant_id = $1 AND communication_id = $2 ORDER BY created_at`, t.tenant, communicationID)
	if err != nil {
		return nil, nf(err)
	}
	defer rows.Close()
	var out []ncd.DeliveryJob
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func (t *ncdTx) HasUnknownAttempt(communicationID string) (bool, error) {
	var ok bool
	err := t.tx.QueryRow(t.ctx, `SELECT EXISTS (SELECT 1 FROM ncd_attempts
		WHERE tenant_id = $1 AND communication_id = $2 AND state = 'UNKNOWN')`, t.tenant, communicationID).Scan(&ok)
	return ok, err
}

// ── NCD-03 attempts ─────────────────────────────────────────────────────────

const attemptCols = `attempt_id::text, tenant_id, job_id::text, communication_id::text, route_index, channel, binding_id,
	intent_id::text, intent_version, purpose_class, recipient_principal_id, origin, idempotency_token, content_hash,
	render_id::text, recipient_snapshot, state, COALESCE(provider_message_id, ''), COALESCE(failure_reason, ''), retryable,
	resolution_due_at, resolved_at, COALESCE(resolved_by_principal_id, ''), COALESCE(resolution_note, ''),
	created_at, submitted_at, state_changed_at`

func scanAttempt(r scannable) (*ncd.Attempt, error) {
	var a ncd.Attempt
	var purpose string
	var snap []byte
	err := r.Scan(&a.AttemptID, &a.TenantID, &a.JobID, &a.CommunicationID, &a.RouteIndex, &a.Channel, &a.BindingID,
		&a.IntentID, &a.IntentVersion, &purpose, &a.RecipientPrincipalID, &a.Origin, &a.IdempotencyToken, &a.ContentHash,
		&a.RenderID, &snap, &a.State, &a.ProviderMessageID, &a.FailureReason, &a.Retryable, &a.ResolutionDueAt,
		&a.ResolvedAt, &a.ResolvedBy, &a.ResolutionNote, &a.CreatedAt, &a.SubmittedAt, &a.StateChangedAt)
	if err != nil {
		return nil, nf(err)
	}
	a.PurposeClass = ncd.PurposeClass(purpose)
	_ = json.Unmarshal(snap, &a.RecipientSnapshot)
	return &a, nil
}

func (t *ncdTx) InsertAttempt(a *ncd.Attempt) error {
	a.TenantID = t.tenant
	err := savepoint(t.ctx, t.tx, func(sp pgx.Tx) error {
		_, err := sp.Exec(t.ctx, `
			INSERT INTO ncd_attempts (attempt_id, tenant_id, job_id, communication_id, route_index, channel, binding_id,
				intent_id, intent_version, purpose_class, recipient_principal_id, origin, idempotency_token, content_hash,
				render_id, recipient_snapshot, state, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
			a.AttemptID, t.tenant, a.JobID, a.CommunicationID, a.RouteIndex, a.Channel, a.BindingID, a.IntentID,
			a.IntentVersion, string(a.PurposeClass), a.RecipientPrincipalID, a.Origin, a.IdempotencyToken, a.ContentHash,
			a.RenderID, mustJSON(a.RecipientSnapshot), a.State, a.CreatedAt)
		return err
	})
	switch pgCode(err) {
	case "P0014", "23505":
		// The UNKNOWN guard (INV-13), or another attempt of this job still
		// open — either way no second material send may start now.
		return ncd.ErrUnknownOutstanding
	}
	return err
}

func (t *ncdTx) GetAttempt(id string) (*ncd.Attempt, error) {
	return scanAttempt(t.tx.QueryRow(t.ctx, `SELECT `+attemptCols+` FROM ncd_attempts
		WHERE tenant_id = $1 AND attempt_id = $2 FOR UPDATE`, t.tenant, id))
}

func (t *ncdTx) UpdateAttempt(a *ncd.Attempt) error {
	_, err := t.tx.Exec(t.ctx, `
		UPDATE ncd_attempts SET state = $3, failure_reason = NULLIF($4, ''), retryable = $5, resolution_due_at = $6,
		       resolved_at = $7, resolved_by_principal_id = NULLIF($8, ''), resolution_note = NULLIF($9, ''),
		       submitted_at = $10
		WHERE tenant_id = $1 AND attempt_id = $2`,
		t.tenant, a.AttemptID, a.State, a.FailureReason, a.Retryable, a.ResolutionDueAt, a.ResolvedAt,
		a.ResolvedBy, a.ResolutionNote, a.SubmittedAt)
	return err
}

func (t *ncdTx) SetProviderMessageID(attemptID, providerMessageID string) error {
	err := savepoint(t.ctx, t.tx, func(sp pgx.Tx) error {
		_, err := sp.Exec(t.ctx, `UPDATE ncd_attempts SET provider_message_id = $3
			WHERE tenant_id = $1 AND attempt_id = $2 AND provider_message_id IS NULL`, t.tenant, attemptID, providerMessageID)
		return err
	})
	if pgCode(err) == "23505" {
		return ncd.ErrProviderIDCollision
	}
	return err
}

func (t *ncdTx) ListAttempts(communicationID string) ([]ncd.Attempt, error) {
	rows, err := t.tx.Query(t.ctx, `SELECT `+attemptCols+` FROM ncd_attempts
		WHERE tenant_id = $1 AND communication_id = $2 ORDER BY created_at`, t.tenant, communicationID)
	if err != nil {
		return nil, nf(err)
	}
	defer rows.Close()
	var out []ncd.Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (t *ncdTx) CountAttempts(since time.Time, recipient, channel, intentID string) (int, error) {
	var n int
	err := t.tx.QueryRow(t.ctx, `SELECT count(*) FROM ncd_attempts
		WHERE tenant_id = $1 AND created_at >= $2
		  AND ($3 = '' OR recipient_principal_id = $3) AND ($4 = '' OR channel = $4)
		  AND ($5 = '' OR intent_id::text = $5)`, t.tenant, since, recipient, channel, intentID).Scan(&n)
	return n, err
}

// ── NCD-04 evidence and exceptions ──────────────────────────────────────────

func (t *ncdTx) InsertEvidence(e *ncd.Evidence) error {
	tag, err := t.tx.Exec(t.ctx, `
		INSERT INTO ncd_delivery_evidence (evidence_id, tenant_id, communication_id, attempt_id, notice_id, evidence_type,
			normalized_state, source, confidence, does_not_prove, binding_id, provider_event_id, payload_hash,
			observed_at, received_at, supersedes_evidence_id, actor_principal_id, details)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),NULLIF($12,''),NULLIF($13,''),$14,$15,$16,NULLIF($17,''),$18)
		ON CONFLICT DO NOTHING`,
		e.EvidenceID, t.tenant, e.CommunicationID, nullStr(e.AttemptID), nullStr(e.NoticeID), e.EvidenceType,
		e.NormalizedState, e.Source, e.Confidence, e.DoesNotProve, e.BindingID, e.ProviderEventID, e.PayloadHash,
		e.ObservedAt, e.ReceivedAt, nullStr(e.SupersedesEvidenceID), e.ActorPrincipalID, mustJSON(e.Details))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ncd.ErrDuplicateEvent
	}
	return nil
}

func (t *ncdTx) ListEvidence(communicationID string) ([]ncd.Evidence, error) {
	rows, err := t.tx.Query(t.ctx, `
		SELECT evidence_id::text, tenant_id, communication_id::text, COALESCE(attempt_id::text, ''), COALESCE(notice_id::text, ''),
		       evidence_type, normalized_state, source, confidence, does_not_prove, COALESCE(binding_id, ''),
		       COALESCE(provider_event_id, ''), COALESCE(payload_hash, ''), observed_at, received_at,
		       COALESCE(supersedes_evidence_id::text, ''), COALESCE(actor_principal_id, ''), details
		FROM ncd_delivery_evidence WHERE tenant_id = $1 AND communication_id = $2 ORDER BY received_at, evidence_id`,
		t.tenant, communicationID)
	if err != nil {
		return nil, nf(err)
	}
	defer rows.Close()
	var out []ncd.Evidence
	for rows.Next() {
		var e ncd.Evidence
		var d []byte
		if err := rows.Scan(&e.EvidenceID, &e.TenantID, &e.CommunicationID, &e.AttemptID, &e.NoticeID, &e.EvidenceType,
			&e.NormalizedState, &e.Source, &e.Confidence, &e.DoesNotProve, &e.BindingID, &e.ProviderEventID, &e.PayloadHash,
			&e.ObservedAt, &e.ReceivedAt, &e.SupersedesEvidenceID, &e.ActorPrincipalID, &d); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(d, &e.Details)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (t *ncdTx) InsertException(x *ncd.Exception) (bool, error) {
	tag, err := t.tx.Exec(t.ctx, `
		INSERT INTO ncd_exceptions (exception_id, tenant_id, communication_id, attempt_id, notice_id, kind, reason_code, detail, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9) ON CONFLICT DO NOTHING`,
		x.ExceptionID, t.tenant, nullStr(x.CommunicationID), nullStr(x.AttemptID), nullStr(x.NoticeID), x.Kind,
		string(x.ReasonCode), x.Detail, x.CreatedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (t *ncdTx) ListExceptions(openOnly bool, limit int) ([]ncd.Exception, error) {
	rows, err := t.tx.Query(t.ctx, `
		SELECT exception_id::text, tenant_id, COALESCE(communication_id::text, ''), COALESCE(attempt_id::text, ''),
		       COALESCE(notice_id::text, ''), kind, COALESCE(reason_code, ''), detail, created_at, resolved_at,
		       COALESCE(resolved_by_principal_id, ''), COALESCE(resolution, '')
		FROM ncd_exceptions WHERE tenant_id = $1 AND (NOT $2 OR resolved_at IS NULL)
		ORDER BY created_at DESC LIMIT $3`, t.tenant, openOnly, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ncd.Exception
	for rows.Next() {
		var x ncd.Exception
		var code string
		if err := rows.Scan(&x.ExceptionID, &x.TenantID, &x.CommunicationID, &x.AttemptID, &x.NoticeID, &x.Kind, &code,
			&x.Detail, &x.CreatedAt, &x.ResolvedAt, &x.ResolvedBy, &x.Resolution); err != nil {
			return nil, err
		}
		x.ReasonCode = ncd.ReasonCode(code)
		out = append(out, x)
	}
	return out, rows.Err()
}

func (t *ncdTx) ResolveException(id, actor, resolution string, at time.Time) error {
	_, err := t.tx.Exec(t.ctx, `UPDATE ncd_exceptions SET resolved_at = $3, resolved_by_principal_id = $4, resolution = $5
		WHERE tenant_id = $1 AND exception_id = $2 AND resolved_at IS NULL`, t.tenant, id, at, actor, resolution)
	return err
}

// ── NCD-05 notices and acknowledgments ──────────────────────────────────────

const noticeCols = `notice_id::text, notice_version, tenant_id, legal_entity_id, communication_id::text, legal_basis_ref,
	recipient_capacity, delivery_methods, content_hash, attachment_manifest, locale, COALESCE(to_char(effective_date, 'YYYY-MM-DD'), ''),
	COALESCE(wfc_obligation_ref, ''), deadline_at, ack_requirement, evidence_requirement, record_requirement, record_status,
	COALESCE(drc_record_ref, ''), state, at_risk_notified_at, COALESCE(supersedes_notice_id::text, ''),
	COALESCE(supersession_reason, ''), COALESCE(disposition_ref, ''), created_by_principal_id, created_at, state_changed_at`

func scanNotice(r scannable) (*ncd.RegulatedNotice, error) {
	var n ncd.RegulatedNotice
	var methods, atts []byte
	var ev string
	err := r.Scan(&n.NoticeID, &n.NoticeVersion, &n.TenantID, &n.LegalEntityID, &n.CommunicationID, &n.LegalBasisRef,
		&n.RecipientCapacity, &methods, &n.ContentHash, &atts, &n.Locale, &n.EffectiveDate, &n.WFCObligationRef,
		&n.DeadlineAt, &n.AckRequirement, &ev, &n.RecordRequirement, &n.RecordStatus, &n.DRCRecordRef, &n.State,
		&n.AtRiskNotifiedAt, &n.SupersedesNoticeID, &n.SupersessionReason, &n.DispositionRef, &n.CreatedByPrincipalID,
		&n.CreatedAt, &n.StateChangedAt)
	if err != nil {
		return nil, nf(err)
	}
	n.EvidenceRequirement = ncd.Level(ev)
	_ = json.Unmarshal(methods, &n.DeliveryMethods)
	_ = json.Unmarshal(atts, &n.AttachmentManifest)
	return &n, nil
}

func (t *ncdTx) InsertNotice(n *ncd.RegulatedNotice) error {
	n.TenantID = t.tenant
	_, err := t.tx.Exec(t.ctx, `
		INSERT INTO ncd_regulated_notices (notice_id, notice_version, tenant_id, legal_entity_id, communication_id,
			legal_basis_ref, recipient_capacity, delivery_methods, content_hash, attachment_manifest, locale, effective_date,
			wfc_obligation_ref, deadline_at, ack_requirement, evidence_requirement, record_requirement, record_status,
			state, supersedes_notice_id, supersession_reason, created_by_principal_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,'')::date,NULLIF($13,''),$14,$15,$16,$17,$18,$19,$20,NULLIF($21,''),$22,$23)`,
		n.NoticeID, n.NoticeVersion, t.tenant, n.LegalEntityID, n.CommunicationID, n.LegalBasisRef, n.RecipientCapacity,
		mustJSON(n.DeliveryMethods), n.ContentHash, mustJSON(n.AttachmentManifest), n.Locale, n.EffectiveDate,
		n.WFCObligationRef, n.DeadlineAt, n.AckRequirement, string(n.EvidenceRequirement), n.RecordRequirement,
		n.RecordStatus, n.State, nullStr(n.SupersedesNoticeID), n.SupersessionReason, n.CreatedByPrincipalID, n.CreatedAt)
	return err
}

func (t *ncdTx) GetNotice(id string, version int) (*ncd.RegulatedNotice, error) {
	if version > 0 {
		return scanNotice(t.tx.QueryRow(t.ctx, `SELECT `+noticeCols+` FROM ncd_regulated_notices
			WHERE tenant_id = $1 AND notice_id = $2 AND notice_version = $3`, t.tenant, id, version))
	}
	return scanNotice(t.tx.QueryRow(t.ctx, `SELECT `+noticeCols+` FROM ncd_regulated_notices
		WHERE tenant_id = $1 AND notice_id = $2 ORDER BY notice_version DESC LIMIT 1`, t.tenant, id))
}

func (t *ncdTx) GetNoticeByCommunication(communicationID string) (*ncd.RegulatedNotice, error) {
	return scanNotice(t.tx.QueryRow(t.ctx, `SELECT `+noticeCols+` FROM ncd_regulated_notices
		WHERE tenant_id = $1 AND communication_id = $2`, t.tenant, communicationID))
}

func (t *ncdTx) ListNoticeVersions(id string) ([]ncd.RegulatedNotice, error) {
	rows, err := t.tx.Query(t.ctx, `SELECT `+noticeCols+` FROM ncd_regulated_notices
		WHERE tenant_id = $1 AND notice_id = $2 ORDER BY notice_version`, t.tenant, id)
	if err != nil {
		return nil, nf(err)
	}
	defer rows.Close()
	var out []ncd.RegulatedNotice
	for rows.Next() {
		n, err := scanNotice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

func (t *ncdTx) UpdateNotice(n *ncd.RegulatedNotice) error {
	_, err := t.tx.Exec(t.ctx, `
		UPDATE ncd_regulated_notices SET state = $4, record_status = $5, drc_record_ref = NULLIF($6, ''),
		       at_risk_notified_at = $7, disposition_ref = NULLIF($8, '')
		WHERE tenant_id = $1 AND notice_id = $2 AND notice_version = $3`,
		t.tenant, n.NoticeID, n.NoticeVersion, n.State, n.RecordStatus, n.DRCRecordRef, n.AtRiskNotifiedAt, n.DispositionRef)
	return err
}

func (t *ncdTx) InsertAck(a *ncd.Acknowledgment) error {
	a.TenantID = t.tenant
	_, err := t.tx.Exec(t.ctx, `
		INSERT INTO ncd_acknowledgments (ack_id, tenant_id, notice_id, notice_version, communication_id, actor_principal_id,
			method, disposition, content_hash, evidence_ref, comment, acknowledged_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),NULLIF($11,''),$12)`,
		a.AckID, t.tenant, a.NoticeID, a.NoticeVersion, a.CommunicationID, a.ActorPrincipalID, a.Method, a.Disposition,
		a.ContentHash, a.EvidenceRef, a.Comment, a.AcknowledgedAt)
	return err
}

func (t *ncdTx) ListAcks(noticeID string) ([]ncd.Acknowledgment, error) {
	rows, err := t.tx.Query(t.ctx, `
		SELECT ack_id::text, tenant_id, notice_id::text, notice_version, communication_id::text, actor_principal_id, method,
		       disposition, content_hash, COALESCE(evidence_ref, ''), COALESCE(comment, ''), acknowledged_at
		FROM ncd_acknowledgments WHERE tenant_id = $1 AND notice_id = $2 ORDER BY acknowledged_at`, t.tenant, noticeID)
	if err != nil {
		return nil, nf(err)
	}
	defer rows.Close()
	var out []ncd.Acknowledgment
	for rows.Next() {
		var a ncd.Acknowledgment
		if err := rows.Scan(&a.AckID, &a.TenantID, &a.NoticeID, &a.NoticeVersion, &a.CommunicationID, &a.ActorPrincipalID,
			&a.Method, &a.Disposition, &a.ContentHash, &a.EvidenceRef, &a.Comment, &a.AcknowledgedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ── maker-checker and bulk ──────────────────────────────────────────────────

const approvalCols = `approval_id::text, tenant_id, legal_entity_id, kind, target_id, payload, reason, status,
	requested_by_principal_id, requested_at, COALESCE(decided_by_principal_id, ''), decided_at, COALESCE(decision_note, '')`

func scanApproval(r scannable) (*ncd.Approval, error) {
	var a ncd.Approval
	var payload []byte
	err := r.Scan(&a.ApprovalID, &a.TenantID, &a.LegalEntityID, &a.Kind, &a.TargetID, &payload, &a.Reason, &a.Status,
		&a.RequestedBy, &a.RequestedAt, &a.DecidedBy, &a.DecidedAt, &a.DecisionNote)
	if err != nil {
		return nil, nf(err)
	}
	_ = json.Unmarshal(payload, &a.Payload)
	return &a, nil
}

func (t *ncdTx) InsertApproval(a *ncd.Approval) error {
	a.TenantID = t.tenant
	if a.LegalEntityID == "" {
		a.LegalEntityID = "-"
	}
	_, err := t.tx.Exec(t.ctx, `
		INSERT INTO ncd_approvals (approval_id, tenant_id, legal_entity_id, kind, target_id, payload, reason, status,
			requested_by_principal_id, requested_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		a.ApprovalID, t.tenant, a.LegalEntityID, a.Kind, a.TargetID, mustJSON(a.Payload), a.Reason, a.Status, a.RequestedBy, a.RequestedAt)
	return err
}

func (t *ncdTx) GetApproval(id string, forUpdate bool) (*ncd.Approval, error) {
	q := `SELECT ` + approvalCols + ` FROM ncd_approvals WHERE tenant_id = $1 AND approval_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	return scanApproval(t.tx.QueryRow(t.ctx, q, t.tenant, id))
}

func (t *ncdTx) UpdateApproval(a *ncd.Approval) error {
	_, err := t.tx.Exec(t.ctx, `
		UPDATE ncd_approvals SET status = $3, decided_by_principal_id = NULLIF($4, ''), decided_at = $5, decision_note = NULLIF($6, '')
		WHERE tenant_id = $1 AND approval_id = $2`, t.tenant, a.ApprovalID, a.Status, a.DecidedBy, a.DecidedAt, a.DecisionNote)
	return err
}

func (t *ncdTx) ListApprovals(targetID string) ([]ncd.Approval, error) {
	rows, err := t.tx.Query(t.ctx, `SELECT `+approvalCols+` FROM ncd_approvals
		WHERE tenant_id = $1 AND target_id = $2 ORDER BY requested_at`, t.tenant, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ncd.Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (t *ncdTx) InsertBulk(b *ncd.BulkSend) error {
	b.TenantID = t.tenant
	_, err := t.tx.Exec(t.ctx, `
		INSERT INTO ncd_bulk_sends (bulk_id, tenant_id, legal_entity_id, intent_id, source_event_id, recipients,
			audience_count, audience_hash, variables, locale, requires_approval, approval_id, status,
			requested_by_principal_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		b.BulkID, t.tenant, b.LegalEntityID, b.IntentID, b.SourceEventID, mustJSON(b.Recipients), b.AudienceCount,
		b.AudienceHash, mustJSON(b.Variables), b.Locale, b.RequiresApproval, nullStr(b.ApprovalID), b.Status,
		b.RequestedBy, b.CreatedAt)
	return err
}

func (t *ncdTx) GetBulk(id string, forUpdate bool) (*ncd.BulkSend, error) {
	q := `SELECT bulk_id::text, tenant_id, legal_entity_id, intent_id::text, source_event_id, recipients, audience_count,
		audience_hash, variables, locale, requires_approval, COALESCE(approval_id::text, ''), status,
		requested_by_principal_id, created_at, dispatched_at
		FROM ncd_bulk_sends WHERE tenant_id = $1 AND bulk_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var b ncd.BulkSend
	var rs, vars []byte
	err := t.tx.QueryRow(t.ctx, q, t.tenant, id).Scan(&b.BulkID, &b.TenantID, &b.LegalEntityID, &b.IntentID, &b.SourceEventID,
		&rs, &b.AudienceCount, &b.AudienceHash, &vars, &b.Locale, &b.RequiresApproval, &b.ApprovalID, &b.Status,
		&b.RequestedBy, &b.CreatedAt, &b.DispatchedAt)
	if err != nil {
		return nil, nf(err)
	}
	if err := json.Unmarshal(rs, &b.Recipients); err != nil {
		return nil, fmt.Errorf("decode bulk recipients: %w", err)
	}
	_ = json.Unmarshal(vars, &b.Variables)
	return &b, nil
}

func (t *ncdTx) UpdateBulk(b *ncd.BulkSend) error {
	_, err := t.tx.Exec(t.ctx, `UPDATE ncd_bulk_sends SET status = $3, dispatched_at = $4
		WHERE tenant_id = $1 AND bulk_id = $2`, t.tenant, b.BulkID, b.Status, b.DispatchedAt)
	return err
}
