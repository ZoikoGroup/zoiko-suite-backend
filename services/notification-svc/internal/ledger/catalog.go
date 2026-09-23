package ledger

// DefaultSeedDefinitions contains the canonical seed templates for Family A and Family B
// defined in ZS-COMMS-EMAIL-001 v2.0 §10 and §11.
func DefaultSeedDefinitions() []TemplateDefinition {
	defs := []TemplateDefinition{
		// ── Family A: Identity and Authentication ──────────────────────────────────
		{
			TemplateKey:        "ZS-IA-001",
			Version:            "1.0.0",
			Locale:             "en-US",
			CommunicationClass: ClassT0,
			SenderStream:       StreamTransactional,
			SubjectTemplate:    "Verify your email for ZoikoSuite",
			HTMLTemplate: `<div style="font-family:sans-serif;max-width:600px;margin:auto;">
  <p style="color:#666;font-size:12px;">Confirm this address to finish securing your account.</p>
  <h2>Verify your email for ZoikoSuite</h2>
  <p>Hi {{index . "recipient.first_name"}},</p>
  <p>Confirm that {{index . "recipient.email_masked"}} belongs to you. The link expires on {{index . "security.link_expires_at_local"}}.</p>
  <p>If you did not create or update a ZoikoSuite account, do not use the link. No account action will be completed until verification succeeds.</p>
  <p style="margin:24px 0;"><a href="{{index . "links.action_url"}}" style="background:#0F172A;color:#FFFFFF;padding:12px 24px;text-decoration:none;border-radius:4px;display:inline-block;">Verify Email Address</a></p>
  <p style="font-size:12px;color:#888;">Ref: {{index . "message.reference"}} | Class: T0</p>
</div>`,
			TextTemplate: `Verify your email for ZoikoSuite

Hi {{index . "recipient.first_name"}},

Confirm that {{index . "recipient.email_masked"}} belongs to you. The link expires on {{index . "security.link_expires_at_local"}}.

If you did not create or update a ZoikoSuite account, do not use the link. No account action will be completed until verification succeeds.

Verify Email Address: {{index . "links.action_url"}}

Ref: {{index . "message.reference"}} | Class: T0`,
			RequiredVariables: []string{
				"recipient.first_name",
				"recipient.email_masked",
				"security.link_expires_at_local",
				"links.action_url",
				"message.reference",
			},
		},
		{
			TemplateKey:        "ZS-IA-004",
			Version:            "1.0.0",
			Locale:             "en-US",
			CommunicationClass: ClassS0,
			SenderStream:       StreamCritical,
			SubjectTemplate:    "Your secure ZoikoSuite sign-in link",
			HTMLTemplate: `<div style="font-family:sans-serif;max-width:600px;margin:auto;">
  <p style="color:#666;font-size:12px;">This single-use sign-in request expires soon.</p>
  <h2>Your secure ZoikoSuite sign-in link</h2>
  <p>Hi {{index . "recipient.first_name"}},</p>
  <p>A secure sign-in link was requested for your ZoikoSuite account. It expires in {{index . "security.expiry_minutes"}} minutes.</p>
  <p>Do not forward this email. Opening the link displays a confirmation page; sign-in completes only after the required verification. If you did not request it, no action is required.</p>
  <p style="margin:24px 0;"><a href="{{index . "links.action_url"}}" style="background:#0F172A;color:#FFFFFF;padding:12px 24px;text-decoration:none;border-radius:4px;display:inline-block;">Sign In Securely</a></p>
  <p style="font-size:12px;color:#888;">Ref: {{index . "message.reference"}} | Class: S0</p>
</div>`,
			TextTemplate: `Your secure ZoikoSuite sign-in link

Hi {{index . "recipient.first_name"}},

A secure sign-in link was requested for your ZoikoSuite account. It expires in {{index . "security.expiry_minutes"}} minutes.

Do not forward this email. Opening the link displays a confirmation page; sign-in completes only after the required verification. If you did not request it, no action is required.

Sign In Securely: {{index . "links.action_url"}}

Ref: {{index . "message.reference"}} | Class: S0`,
			RequiredVariables: []string{
				"recipient.first_name",
				"security.expiry_minutes",
				"links.action_url",
				"message.reference",
			},
		},
		{
			TemplateKey:        "ZS-IA-005",
			Version:            "1.0.0",
			Locale:             "en-US",
			CommunicationClass: ClassS0,
			SenderStream:       StreamCritical,
			SubjectTemplate:    "Reset your ZoikoSuite password",
			HTMLTemplate: `<div style="font-family:sans-serif;max-width:600px;margin:auto;">
  <p style="color:#666;font-size:12px;">Use this link only if you requested a password reset.</p>
  <h2>Reset your ZoikoSuite password</h2>
  <p>Hi {{index . "recipient.first_name"}},</p>
  <p>We received a request to reset your ZoikoSuite password. The secure reset request expires in {{index . "security.expiry_minutes"}} minutes.</p>
  <p>If you did not request the reset, do not use the link. Review Account Security if unexpected requests continue.</p>
  <p style="margin:24px 0;"><a href="{{index . "links.action_url"}}" style="background:#0F172A;color:#FFFFFF;padding:12px 24px;text-decoration:none;border-radius:4px;display:inline-block;">Reset Password</a></p>
  <p style="font-size:12px;color:#888;">Ref: {{index . "message.reference"}} | Class: S0</p>
</div>`,
			TextTemplate: `Reset your ZoikoSuite password

Hi {{index . "recipient.first_name"}},

We received a request to reset your ZoikoSuite password. The secure reset request expires in {{index . "security.expiry_minutes"}} minutes.

If you did not request the reset, do not use the link. Review Account Security if unexpected requests continue.

Reset Password: {{index . "links.action_url"}}

Ref: {{index . "message.reference"}} | Class: S0`,
			RequiredVariables: []string{
				"recipient.first_name",
				"security.expiry_minutes",
				"links.action_url",
				"message.reference",
			},
		},

		// ── Family B: Organizations and Workspaces ─────────────────────────────────
		{
			TemplateKey:        "ZS-OW-017",
			Version:            "1.0.0",
			Locale:             "en-US",
			CommunicationClass: ClassT0,
			SenderStream:       StreamTransactional,
			SubjectTemplate:    "You're invited to join {{index . \"organization.name\"}}",
			HTMLTemplate: `<div style="font-family:sans-serif;max-width:600px;margin:auto;">
  <p style="color:#666;font-size:12px;">Accept your invitation to the ZoikoSuite workspace.</p>
  <h2>You're invited to join {{index . "organization.name"}}</h2>
  <p>Hi {{index . "recipient.first_name"}},</p>
  <p>{{index . "actor.display_name"}} invited you to join {{index . "organization.name"}}.</p>
  <p>Workspace: {{index . "workspace.name"}}. Role: {{index . "recipient.proposed_role"}}. The invitation expires on {{index . "security.link_expires_at_local"}}. Access is limited to the products and capabilities authorized for the role.</p>
  <p style="margin:24px 0;"><a href="{{index . "links.action_url"}}" style="background:#0F172A;color:#FFFFFF;padding:12px 24px;text-decoration:none;border-radius:4px;display:inline-block;">Accept Invitation</a></p>
  <p style="font-size:12px;color:#888;">Ref: {{index . "message.reference"}} | Class: T0</p>
</div>`,
			TextTemplate: `You're invited to join {{index . "organization.name"}}

Hi {{index . "recipient.first_name"}},

{{index . "actor.display_name"}} invited you to join {{index . "organization.name"}}.

Workspace: {{index . "workspace.name"}}. Role: {{index . "recipient.proposed_role"}}. The invitation expires on {{index . "security.link_expires_at_local"}}. Access is limited to the products and capabilities authorized for the role.

Accept Invitation: {{index . "links.action_url"}}

Ref: {{index . "message.reference"}} | Class: T0`,
			RequiredVariables: []string{
				"recipient.first_name",
				"actor.display_name",
				"organization.name",
				"workspace.name",
				"recipient.proposed_role",
				"security.link_expires_at_local",
				"links.action_url",
				"message.reference",
			},
		},
		{
			TemplateKey:        "ZS-OW-021",
			Version:            "1.0.0",
			Locale:             "en-US",
			CommunicationClass: ClassS0,
			SenderStream:       StreamCritical,
			SubjectTemplate:    "Your ZoikoSuite access role changed",
			HTMLTemplate: `<div style="font-family:sans-serif;max-width:600px;margin:auto;">
  <p style="color:#666;font-size:12px;">Your permissions for {{index . "organization.name"}} were updated.</p>
  <h2>Your ZoikoSuite access role changed</h2>
  <p>Hi {{index . "recipient.first_name"}},</p>
  <p>Your role in {{index . "organization.name"}} changed on {{index . "event.occurred_at_local"}}.</p>
  <p>Previous role: {{index . "recipient.previous_role"}}. New role: {{index . "recipient.new_role"}}. Changed by: {{index . "actor.display_name"}}. Available products, data and actions may have changed.</p>
  <p style="margin:24px 0;"><a href="{{index . "links.action_url"}}" style="background:#0F172A;color:#FFFFFF;padding:12px 24px;text-decoration:none;border-radius:4px;display:inline-block;">Review My Access</a></p>
  <p style="font-size:12px;color:#888;">Ref: {{index . "message.reference"}} | Class: S0</p>
</div>`,
			TextTemplate: `Your ZoikoSuite access role changed

Hi {{index . "recipient.first_name"}},

Your role in {{index . "organization.name"}} changed on {{index . "event.occurred_at_local"}}.

Previous role: {{index . "recipient.previous_role"}}. New role: {{index . "recipient.new_role"}}. Changed by: {{index . "actor.display_name"}}. Available products, data and actions may have changed.

Review My Access: {{index . "links.action_url"}}

Ref: {{index . "message.reference"}} | Class: S0`,
			RequiredVariables: []string{
				"recipient.first_name",
				"organization.name",
				"event.occurred_at_local",
				"recipient.previous_role",
				"recipient.new_role",
				"actor.display_name",
				"links.action_url",
				"message.reference",
			},
		},
	}

	// Calculate and assign deterministic expected SHA256 hashes
	for i := range defs {
		defs[i].ExpectedSHA256Hash = ComputeContentHash(
			defs[i].TemplateKey,
			defs[i].Version,
			defs[i].Locale,
			defs[i].SubjectTemplate,
			defs[i].HTMLTemplate,
			defs[i].TextTemplate,
		)
	}

	return defs
}
