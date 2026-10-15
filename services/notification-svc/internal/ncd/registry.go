package ncd

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ── NCD-01 Communication Intent registry (§4.2) ─────────────────────────────

// IntentInput is the author-supplied part of an intent version.
type IntentInput struct {
	LegalEntityID             string           `json:"legal_entity_id"`
	IntentCode                string           `json:"intent_code"`
	DisplayName               string           `json:"display_name"`
	PurposeClass              PurposeClass     `json:"purpose_class"`
	DomainOwner               string           `json:"domain_owner"`
	Sensitivity               Level            `json:"sensitivity"`
	Urgency                   Level            `json:"urgency"`
	EvidenceClass             Level            `json:"evidence_class"`
	AllowedChannels           []string         `json:"allowed_channels"`
	FallbackAllowed           bool             `json:"fallback_allowed"`
	MarketingAllowed          bool             `json:"marketing_allowed"`
	Mandatory                 bool             `json:"mandatory"`
	PreferenceOverrideAllowed bool             `json:"preference_override_allowed"`
	QuietHoursPolicy          string           `json:"quiet_hours_policy,omitempty"`
	BulkAllowed               bool             `json:"bulk_allowed"`
	RecordRequirement         bool             `json:"record_requirement"`
	AckRequirement            string           `json:"ack_requirement,omitempty"`
	VariableContract          []VariableSpec   `json:"variable_contract"`
	AttachmentContract        []AttachmentSlot `json:"attachment_contract,omitempty"`
	ApprovedURLDomains        []string         `json:"approved_url_domains,omitempty"`
	DefaultExpirySeconds      int              `json:"default_expiry_seconds,omitempty"`
}

var (
	identRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

func (in *IntentInput) normalize() {
	if in.QuietHoursPolicy == "" {
		in.QuietHoursPolicy = "RESPECT"
	}
	if in.AckRequirement == "" {
		in.AckRequirement = AckNone
	}
	if in.DefaultExpirySeconds == 0 {
		in.DefaultExpirySeconds = 7 * 24 * 3600
	}
	if in.PurposeClass == PurposeMarketing {
		// A marketing intent is marketing; §4.2's flag cannot be false for it.
		in.MarketingAllowed = true
	}
	for i := range in.ApprovedURLDomains {
		in.ApprovedURLDomains[i] = strings.ToLower(strings.TrimSpace(in.ApprovedURLDomains[i]))
	}
	for i := range in.VariableContract {
		if in.VariableContract[i].Sensitivity == "" {
			in.VariableContract[i].Sensitivity = "S0"
		}
		if in.VariableContract[i].Type == "" {
			in.VariableContract[i].Type = "string"
		}
	}
}

func (in IntentInput) validate() *Error {
	switch {
	case in.LegalEntityID == "", strings.TrimSpace(in.IntentCode) == "", strings.TrimSpace(in.DisplayName) == "",
		strings.TrimSpace(in.DomainOwner) == "":
		return Invalid("missing_fields", "legal_entity_id, intent_code, display_name and domain_owner are required")
	case !in.PurposeClass.Valid():
		return Invalid("invalid_purpose_class", "purpose_class %q is not one of the six §2.2 classes", in.PurposeClass)
	case !ValidSensitivity(in.Sensitivity):
		return Invalid("invalid_sensitivity", "sensitivity must be S0..S3")
	case !ValidUrgency(in.Urgency):
		return Invalid("invalid_urgency", "urgency must be U0..U3")
	case !ValidEvidenceClass(in.EvidenceClass):
		return Invalid("invalid_evidence_class", "evidence_class must be E0..E4")
	case len(in.AllowedChannels) == 0:
		return Invalid("missing_fields", "allowed_channels needs at least one channel")
	case in.QuietHoursPolicy != "RESPECT" && in.QuietHoursPolicy != "EXEMPT":
		return Invalid("invalid_quiet_hours_policy", "quiet_hours_policy must be RESPECT or EXEMPT")
	case !ValidAckRequirement(in.AckRequirement):
		return Invalid("invalid_ack_requirement", "ack_requirement must be NONE, RECEIPT, AUTHENTICATED_ACK or ACCEPTANCE")
	case in.DefaultExpirySeconds < 60:
		return Invalid("invalid_expiry", "default_expiry_seconds must be at least 60")
	}
	for _, c := range in.AllowedChannels {
		if !ValidChannel(c) {
			return Invalid("invalid_channel", "channel %q is not EMAIL, IN_APP, SMS or PUSH", c)
		}
	}
	if in.MarketingAllowed && (in.PurposeClass == PurposeSecurityCritical || in.PurposeClass == PurposeRegulated) {
		return Invalid("marketing_not_permitted",
			"marketing_allowed cannot be true for a %s intent (§4.2)", in.PurposeClass)
	}
	if in.Mandatory && in.PurposeClass == PurposeMarketing {
		return Invalid("mandatory_marketing", "a marketing intent cannot be mandatory")
	}
	if in.PreferenceOverrideAllowed && !in.Mandatory {
		return Invalid("override_needs_mandatory", "preference_override_allowed applies only to a mandatory intent (§5.3)")
	}
	if in.PurposeClass == PurposeRegulated && in.EvidenceClass.Rank() < 2 {
		return Invalid("evidence_class_too_low",
			"a REGULATED_RIGHTS_AFFECTING intent needs evidence class E2 or higher (§2.2: high evidence)")
	}
	if in.AckRequirement != AckNone && in.PurposeClass != PurposeRegulated {
		return Invalid("ack_requirement_needs_regulated", "acknowledgment requirements belong to regulated notices (NCD-05)")
	}
	if in.BulkAllowed && in.PurposeClass == PurposeRegulated {
		return Invalid("bulk_not_permitted", "a regulated notice cannot be bulk-sent (§6.5)")
	}
	seen := map[string]bool{}
	for _, v := range in.VariableContract {
		if !identRE.MatchString(v.Name) {
			return Invalid("invalid_variable_contract", "variable name %q is not an identifier", v.Name)
		}
		if seen[v.Name] {
			return Invalid("invalid_variable_contract", "variable %q is declared twice", v.Name)
		}
		seen[v.Name] = true
		if !ValidVariableType(v.Type) {
			return Invalid("invalid_variable_contract", "variable %q has unknown type %q", v.Name, v.Type)
		}
		if !ValidSensitivity(v.Sensitivity) {
			return Invalid("invalid_variable_contract", "variable %q sensitivity must be S0..S3", v.Name)
		}
		if v.Type == "enum" && len(v.AllowedValues) == 0 {
			return Invalid("invalid_variable_contract", "enum variable %q needs allowed_values", v.Name)
		}
		if v.Required && v.FallbackText != "" {
			return Invalid("invalid_variable_contract", "required variable %q cannot have fallback text; a required value is never replaced", v.Name)
		}
	}
	slots := map[string]bool{}
	for _, a := range in.AttachmentContract {
		if !identRE.MatchString(a.Slot) || a.DRCRecordType == "" || slots[a.Slot] {
			return Invalid("invalid_attachment_contract", "attachment slot %q needs a unique identifier and a drc_record_type", a.Slot)
		}
		if a.MaxSensitivity != "" && !ValidSensitivity(a.MaxSensitivity) {
			return Invalid("invalid_attachment_contract", "slot %q max_sensitivity must be S0..S3", a.Slot)
		}
		slots[a.Slot] = true
	}
	for _, d := range in.ApprovedURLDomains {
		if !domainRE.MatchString(d) {
			return Invalid("invalid_url_domain", "approved_url_domains entry %q is not a hostname", d)
		}
	}
	return nil
}

func (in IntentInput) toIntent() Intent {
	return Intent{
		LegalEntityID: in.LegalEntityID, IntentCode: in.IntentCode, DisplayName: in.DisplayName,
		PurposeClass: in.PurposeClass, DomainOwner: in.DomainOwner, Sensitivity: in.Sensitivity,
		Urgency: in.Urgency, EvidenceClass: in.EvidenceClass, AllowedChannels: in.AllowedChannels,
		FallbackAllowed: in.FallbackAllowed, MarketingAllowed: in.MarketingAllowed, Mandatory: in.Mandatory,
		PreferenceOverrideAllowed: in.PreferenceOverrideAllowed, QuietHoursPolicy: in.QuietHoursPolicy,
		BulkAllowed: in.BulkAllowed, RecordRequirement: in.RecordRequirement, AckRequirement: in.AckRequirement,
		VariableContract: nonNilSpecs(in.VariableContract), AttachmentContract: nonNilSlots(in.AttachmentContract),
		ApprovedURLDomains: nonNilStrings(in.ApprovedURLDomains), DefaultExpirySeconds: in.DefaultExpirySeconds,
	}
}

func nonNilSpecs(v []VariableSpec) []VariableSpec {
	if v == nil {
		return []VariableSpec{}
	}
	return v
}
func nonNilSlots(v []AttachmentSlot) []AttachmentSlot {
	if v == nil {
		return []AttachmentSlot{}
	}
	return v
}
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// CreateIntent creates a DRAFT intent, version 1, with a server-assigned id.
func (s *Service) CreateIntent(ctx context.Context, a Actor, in IntentInput) (*Intent, error) {
	in.normalize()
	if err := in.validate(); err != nil {
		return nil, err
	}
	i := in.toIntent()
	i.IntentID = uuid.NewString()
	i.Version = 1
	i.Status = "DRAFT"
	i.CreatedByPrincipalID = a.PrincipalID
	i.CreatedAt = s.now()
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error { return tx.InsertIntent(&i) })
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// CreateIntentVersion drafts the next version of an intent. Any change to an
// intent is a new version; an existing version never changes (§4.2).
func (s *Service) CreateIntentVersion(ctx context.Context, a Actor, intentID string, in IntentInput) (*Intent, error) {
	in.normalize()
	if err := in.validate(); err != nil {
		return nil, err
	}
	var out Intent
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		latest, err := tx.GetIntent(intentID, 0)
		if err != nil {
			return mapNotFound(err, "intent_not_found", "intent %s", intentID)
		}
		if latest.Status == "RETIRED" {
			return Conflict("intent_retired", "intent %s is retired", intentID)
		}
		if latest.LegalEntityID != in.LegalEntityID {
			return Invalid("legal_entity_mismatch", "a new version cannot move the intent to another legal entity")
		}
		out = in.toIntent()
		out.IntentID = intentID
		out.Version = latest.Version + 1
		out.Status = "DRAFT"
		out.CreatedByPrincipalID = a.PrincipalID
		out.CreatedAt = s.now()
		return tx.InsertIntent(&out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetIntent returns one version (0 = latest) and the version history.
func (s *Service) GetIntent(ctx context.Context, a Actor, intentID string, version int) (*Intent, []Intent, error) {
	var i *Intent
	var all []Intent
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		if i, err = tx.GetIntent(intentID, version); err != nil {
			return mapNotFound(err, "intent_not_found", "intent %s", intentID)
		}
		all, err = tx.ListIntentVersions(intentID)
		return err
	})
	return i, all, err
}

// ActivateIntent approves and activates a DRAFT version. The approver is the
// caller — never a name in the body — and may not be its author (SoD).
func (s *Service) ActivateIntent(ctx context.Context, a Actor, intentID string, version int, effectiveFrom *time.Time) (*Intent, error) {
	now := s.now()
	eff := now
	if effectiveFrom != nil {
		if effectiveFrom.Before(now.Add(-time.Minute)) {
			return nil, Invalid("effective_from_in_past", "effective_from cannot be in the past; history is not rewritten (§9.2)")
		}
		eff = effectiveFrom.UTC()
	}
	var out *Intent
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		i, err := tx.GetIntent(intentID, version)
		if err != nil {
			return mapNotFound(err, "intent_not_found", "intent %s version %d", intentID, version)
		}
		if i.Status != "DRAFT" {
			return Conflict("not_draft", "intent %s version %d is %s", intentID, i.Version, i.Status)
		}
		if i.CreatedByPrincipalID == a.PrincipalID {
			return Forbidden("self_approval_forbidden", "the author of an intent version cannot activate it")
		}
		if err := tx.ActivateIntent(intentID, i.Version, a.PrincipalID, now, eff); err != nil {
			return err
		}
		out, err = tx.GetIntent(intentID, i.Version)
		return err
	})
	return out, err
}

// RetireIntent retires every version; nothing new may be prepared under it.
func (s *Service) RetireIntent(ctx context.Context, a Actor, intentID string) (*Intent, error) {
	var out *Intent
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		if _, err := tx.GetIntent(intentID, 0); err != nil {
			return mapNotFound(err, "intent_not_found", "intent %s", intentID)
		}
		n, err := tx.RetireIntent(intentID, a.PrincipalID, s.now())
		if err != nil {
			return err
		}
		if n == 0 {
			return Conflict("already_retired", "intent %s is already retired", intentID)
		}
		out, err = tx.GetIntent(intentID, 0)
		return err
	})
	return out, err
}

// Effective resolves GET /v1/intents/{id}/effective.
func (s *Service) Effective(ctx context.Context, a Actor, intentID string, txTime, knownAt time.Time) (*EffectiveSet, error) {
	var out *EffectiveSet
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		i, err := tx.EffectiveIntent(intentID, txTime, knownAt)
		if errors.Is(err, ErrNotFound) {
			if _, e2 := tx.GetIntent(intentID, 0); e2 != nil {
				return Refused(Refuse(NCD001IntentNotFound, "no intent "+intentID))
			}
			return Refused(Refuse(NCD002IntentNotEffective, "no version of the intent is effective at the requested transaction/knowledge time"))
		}
		if err != nil {
			return err
		}
		ts, err := tx.EffectiveTemplates(intentID, txTime, knownAt)
		if err != nil {
			return err
		}
		if ts == nil {
			ts = []TemplateVersion{}
		}
		out = &EffectiveSet{IntentID: intentID, TransactionTime: txTime, KnowledgeTime: knownAt, Intent: *i, Templates: ts}
		return nil
	})
	return out, err
}

// ── templates (§4.3) ────────────────────────────────────────────────────────

// TemplateInput drafts a template version.
type TemplateInput struct {
	IntentID          string   `json:"intent_id"`
	Channel           string   `json:"channel"`
	Locale            string   `json:"locale"`
	CompatibleLocales []string `json:"compatible_locales,omitempty"`
	Subject           string   `json:"subject"`
	Body              string   `json:"body"`
}

// authoringIntent is the intent version a template is written and validated
// against: the latest ACTIVE version, else the latest DRAFT.
func authoringIntent(tx Tx, intentID string) (*Intent, error) {
	vs, err := tx.ListIntentVersions(intentID)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, NotFound("intent_not_found", "intent %s", intentID)
	}
	var draft *Intent
	for i := len(vs) - 1; i >= 0; i-- {
		v := vs[i]
		if v.Status == "ACTIVE" {
			return &v, nil
		}
		if v.Status == "DRAFT" && draft == nil {
			draft = &v
		}
	}
	if draft != nil {
		return draft, nil
	}
	return nil, Conflict("intent_retired", "intent %s is retired", intentID)
}

// CreateTemplate drafts a template version bound to intent/channel/locale.
func (s *Service) CreateTemplate(ctx context.Context, a Actor, in TemplateInput) (*TemplateVersion, error) {
	if in.IntentID == "" || in.Channel == "" || in.Locale == "" || strings.TrimSpace(in.Body) == "" {
		return nil, Invalid("missing_fields", "intent_id, channel, locale and body are required")
	}
	if !ValidChannel(in.Channel) {
		return nil, Invalid("invalid_channel", "channel %q is not EMAIL, IN_APP, SMS or PUSH", in.Channel)
	}
	if !ValidLocale(in.Locale) {
		return nil, Invalid("invalid_locale", "locale %q is not a locale tag like en or en-GB", in.Locale)
	}
	var out TemplateVersion
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		intent, err := authoringIntent(tx, in.IntentID)
		if err != nil {
			return mapNotFound(err, "intent_not_found", "intent %s", in.IntentID)
		}
		if !intent.Allows(in.Channel) {
			return Invalid("channel_not_allowed", "intent %s does not allow %s", in.IntentID, in.Channel)
		}
		tid, ver, err := tx.NextTemplateSlot(in.IntentID, in.Channel, in.Locale)
		if err != nil {
			return err
		}
		out = TemplateVersion{
			TemplateVersionID: uuid.NewString(), TemplateID: tid, Version: ver,
			LegalEntityID: intent.LegalEntityID, IntentID: in.IntentID, IntentVersion: intent.Version,
			Channel: in.Channel, Locale: in.Locale, CompatibleLocales: nonNilStrings(in.CompatibleLocales),
			Subject: in.Subject, Body: in.Body,
			ContentHash: ContentHash(in.Channel, in.Locale, in.Subject, in.Body),
			SchemaHash:  SchemaHash(in.Subject, in.Body),
			Status:      TemplateDraft, CreatedByPrincipalID: a.PrincipalID, CreatedAt: s.now(),
		}
		return tx.InsertTemplate(&out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTemplate returns one template version.
func (s *Service) GetTemplate(ctx context.Context, a Actor, id string) (*TemplateVersion, error) {
	var out *TemplateVersion
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		out, err = tx.GetTemplate(id)
		return mapNotFound(err, "template_not_found", "template version %s", id)
	})
	return out, err
}

// ListTemplates returns every template version of an intent.
func (s *Service) ListTemplates(ctx context.Context, a Actor, intentID string) ([]TemplateVersion, error) {
	var out []TemplateVersion
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		out, err = tx.ListTemplates(intentID)
		return err
	})
	if out == nil {
		out = []TemplateVersion{}
	}
	return out, err
}

// ValidateTemplate runs §4.4 validation. A DRAFT that passes moves to REVIEW;
// one that fails stays DRAFT with its report, which is returned either way.
func (s *Service) ValidateTemplate(ctx context.Context, a Actor, id string) (*TemplateVersion, error) {
	var out *TemplateVersion
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		tv, err := tx.GetTemplate(id)
		if err != nil {
			return mapNotFound(err, "template_not_found", "template version %s", id)
		}
		if tv.Status != TemplateDraft {
			return Conflict("not_draft", "template version %s is %s; only a DRAFT is validated", id, tv.Status)
		}
		intent, err := authoringIntent(tx, tv.IntentID)
		if err != nil {
			return err
		}
		now := s.now()
		report := ValidateTemplate(*intent, *tv, now)
		tv.ValidationReport = &report
		tv.ValidatedAt = &now
		if report.Passed {
			tv.Status = TemplateReview
		}
		if err := tx.UpdateTemplate(tv); err != nil {
			return err
		}
		out = tv
		return nil
	})
	return out, err
}

// ApproveTemplate approves a REVIEW version. The approver may not be its
// author (§4.5 "cannot approve own material change").
func (s *Service) ApproveTemplate(ctx context.Context, a Actor, id string) (*TemplateVersion, error) {
	return s.transitionTemplate(ctx, a, id, func(tv *TemplateVersion, now time.Time) error {
		if tv.Status != TemplateReview {
			return Conflict("not_review", "template version %s is %s; approve needs REVIEW", id, tv.Status)
		}
		if tv.ValidationReport == nil || !tv.ValidationReport.Passed {
			return Conflict("not_validated", "template version %s has no passing validation report", id)
		}
		if tv.CreatedByPrincipalID == a.PrincipalID {
			return Forbidden("self_approval_forbidden", "the author of a template version cannot approve it")
		}
		tv.Status = TemplateApproved
		tv.ApprovedByPrincipalID = a.PrincipalID
		tv.ApprovedAt = &now
		return nil
	})
}

// RejectTemplate returns a REVIEW version to DRAFT (§4.3 "reject → DRAFT").
func (s *Service) RejectTemplate(ctx context.Context, a Actor, id string) (*TemplateVersion, error) {
	return s.transitionTemplate(ctx, a, id, func(tv *TemplateVersion, now time.Time) error {
		if tv.Status != TemplateReview {
			return Conflict("not_review", "template version %s is %s; reject needs REVIEW", id, tv.Status)
		}
		tv.Status = TemplateDraft
		return nil
	})
}

// PublishTemplate publishes an APPROVED version, effective-dated and
// immutable from then on.
func (s *Service) PublishTemplate(ctx context.Context, a Actor, id string, effectiveFrom *time.Time) (*TemplateVersion, error) {
	return s.transitionTemplate(ctx, a, id, func(tv *TemplateVersion, now time.Time) error {
		if tv.Status != TemplateApproved {
			return Conflict("not_approved", "template version %s is %s; publish needs APPROVED", id, tv.Status)
		}
		eff := now
		if effectiveFrom != nil {
			if effectiveFrom.Before(now.Add(-time.Minute)) {
				return Invalid("effective_from_in_past", "effective_from cannot be in the past")
			}
			eff = effectiveFrom.UTC()
		}
		tv.Status = TemplatePublished
		tv.PublishedByPrincipalID = a.PrincipalID
		tv.PublishedAt = &now
		tv.EffectiveFrom = &eff
		return nil
	})
}

// RetireTemplate retires a version (§13.2 "retire/quarantine affected version").
func (s *Service) RetireTemplate(ctx context.Context, a Actor, id string) (*TemplateVersion, error) {
	return s.transitionTemplate(ctx, a, id, func(tv *TemplateVersion, now time.Time) error {
		if tv.Status == TemplateRetired {
			return Conflict("already_retired", "template version %s is already retired", id)
		}
		tv.Status = TemplateRetired
		tv.RetiredAt = &now
		return nil
	})
}

func (s *Service) transitionTemplate(ctx context.Context, a Actor, id string, fn func(*TemplateVersion, time.Time) error) (*TemplateVersion, error) {
	var out *TemplateVersion
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		tv, err := tx.GetTemplate(id)
		if err != nil {
			return mapNotFound(err, "template_not_found", "template version %s", id)
		}
		if err := fn(tv, s.now()); err != nil {
			return err
		}
		if err := tx.UpdateTemplate(tv); err != nil {
			return err
		}
		out = tv
		return nil
	})
	return out, err
}

// PreviewInput is POST /v1/render-previews.
type PreviewInput struct {
	TemplateVersionID string            `json:"template_version_id"`
	Variables         map[string]string `json:"variables,omitempty"`
}

// Preview is a side-effect-free render.
type Preview struct {
	TemplateVersionID string            `json:"template_version_id"`
	Status            string            `json:"template_status"`
	Synthetic         bool              `json:"synthetic_data"`
	Redacted          []string          `json:"redacted_variables"`
	Subject           string            `json:"subject"`
	Body              string            `json:"body"`
	ContentHash       string            `json:"content_hash"`
	Refusal           *Refusal          `json:"refusal,omitempty"`
	Variables         map[string]string `json:"variables_used"`
}

// RenderPreview renders any version against synthetic data — or against
// supplied values with every S2/S3 value redacted — and writes nothing
// (§4.5, §11.3 "previews use synthetic or explicitly authorized data").
func (s *Service) RenderPreview(ctx context.Context, a Actor, in PreviewInput) (*Preview, error) {
	var out *Preview
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		tv, err := tx.GetTemplate(in.TemplateVersionID)
		if err != nil {
			return mapNotFound(err, "template_not_found", "template version %s", in.TemplateVersionID)
		}
		intent, err := authoringIntent(tx, tv.IntentID)
		if err != nil {
			return err
		}
		vars := SyntheticVariables(*intent)
		p := &Preview{TemplateVersionID: tv.TemplateVersionID, Status: tv.Status, Synthetic: len(in.Variables) == 0, Redacted: []string{}}
		if len(in.Variables) > 0 {
			red := RedactVariables(*intent, in.Variables)
			for k, v := range red {
				if v != in.Variables[k] {
					p.Redacted = append(p.Redacted, k)
				}
				vars[k] = v
			}
		}
		p.Variables = vars
		r, ref := Render(*intent, *tv, vars)
		if ref != nil {
			p.Refusal = ref
		} else {
			p.Subject, p.Body, p.ContentHash = r.Subject, r.Body, r.ContentHash
		}
		out = p
		return nil
	})
	return out, err
}

func mapNotFound(err error, code, format string, a ...any) error {
	if errors.Is(err, ErrNotFound) {
		return NotFound(code, format, a...)
	}
	return err
}
