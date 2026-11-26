// Package notify decides who is told what when a workflow moves, and in which
// words (Doc 03 §9.7: notifications for workflows, deadlines, escalations and
// approvals).
//
// It is a pure function of the transition. The store calls Build inside the
// transition's own transaction and writes each Notice to the outbox under
// destination "notification"; a relay hands them to notification-svc. A
// notice that cannot be delivered never undoes the decision that caused it
// (§9.7: "notification failure must not collapse source operational
// workflows").
//
// Each recipient gets the notice twice: IN_APP, which is the console's bell,
// and EMAIL. The bell shows the subject only and carries no link, so every
// subject names what happened and the body says where to act.
package notify

import (
	"fmt"
	"html"
	"strings"
)

// Moment is the workflow transition a notice reports.
type Moment string

// The moments that notify someone (plan "Who is told what", 26 Nov 2026).
const (
	// Started: the workflow was created; stage 1 waits on its approvers.
	Started Moment = "started"
	// StageApproved: a stage was approved and a later stage now waits.
	StageApproved Moment = "stage_approved"
	// Approved: the last stage was approved; the workflow is APPROVED.
	Approved Moment = "approved"
	// Rejected: a stage was rejected; the workflow is REJECTED.
	Rejected Moment = "rejected"
	// Reopened: changes were requested and a new review cycle starts at stage 1.
	Reopened Moment = "reopened"
	// Escalated: the workflow was escalated, to a named target or not.
	Escalated Moment = "escalated"
	// Expired: the workflow passed its deadline undecided.
	Expired Moment = "expired"
	// Cancelled: the workflow was withdrawn or cancelled.
	Cancelled Moment = "cancelled"
	// Invalidated: an approval no longer covers its subject.
	Invalidated Moment = "invalidated"
)

// Channels a notice is sent on, as notification-svc names them.
const (
	ChannelInApp = "IN_APP"
	ChannelEmail = "EMAIL"
)

// maxReason bounds the reason quoted in a body. The full text stays on the
// workflow's transition record.
const maxReason = 1000

// Input is everything one transition needs to be reported.
type Input struct {
	Moment             Moment
	WorkflowInstanceID string
	WorkflowType       string
	SubjectType        string
	SubjectID          string
	InitiatedBy        string
	// Actor is who caused the transition. They are never notified of it.
	Actor string
	// Rationale is the reason a person gave, if any. Synthetic bookkeeping
	// text ("PENDING -> CANCELLED by x") must not be passed: it is not a reason.
	Rationale string
	// StageOrder is the stage that was decided (StageApproved).
	StageOrder int
	// Approvers are the approver-side recipients: the stage that now waits
	// (Started, StageApproved, Reopened), the escalation target (Escalated),
	// or the stage that was waiting (Cancelled, Invalidated).
	Approvers []string
	// BaseURL is the console's public base URL. Empty gives a relative link.
	BaseURL string
}

// Notice is one message for one recipient on one channel.
type Notice struct {
	RecipientPrincipalID string `json:"recipient_principal_id"`
	Channel              string `json:"channel"`
	Subject              string `json:"subject"`
	Body                 string `json:"body"`
	// SourceEventType is what the bell shows under the subject.
	SourceEventType string `json:"source_event_type"`
	// SourceReference is the workflow instance the notice is about.
	SourceReference string `json:"source_reference"`
}

// message is one wording, before it is fanned out to recipients and channels.
type message struct {
	recipients []string
	subject    string
	body       string
	eventType  string
}

// Build returns the notices for one transition. Nobody is notified of their
// own action, a recipient is told once per message, and service principals
// (which read no bell and have no mailbox) are skipped.
func Build(in Input) []Notice {
	title := Title(in.WorkflowType, in.SubjectType, in.SubjectID)
	link := Link(in.BaseURL, in.WorkflowInstanceID)
	reason := clip(strings.TrimSpace(in.Rationale), maxReason)
	requester := []string{in.InitiatedBy}

	var msgs []message
	switch in.Moment {
	case Started:
		msgs = append(msgs, waitingOn(in.Approvers, title, link, "workflow.approval_requested",
			"Approval needed: "+title, "A request is waiting for your decision."))
	case StageApproved:
		msgs = append(msgs,
			waitingOn(in.Approvers, title, link, "workflow.approval_requested",
				"Approval needed: "+title, fmt.Sprintf("Stage %d was approved. The request now waits for your decision.", in.StageOrder)),
			message{recipients: requester, eventType: "workflow.stage_approved",
				subject: fmt.Sprintf("Stage %d approved: %s", in.StageOrder, title),
				body: requesterBody(fmt.Sprintf("Stage %d of your request \"%s\" was approved. It now waits on the next approver.", in.StageOrder, title),
					reason, "Comment", link)})
	case Approved:
		msgs = append(msgs, message{recipients: requester, eventType: "workflow.approved",
			subject: "Approved: " + title,
			body:    requesterBody(fmt.Sprintf("Your request \"%s\" was approved.", title), reason, "Comment", link)})
	case Rejected:
		msgs = append(msgs, message{recipients: requester, eventType: "workflow.rejected",
			subject: "Rejected: " + title,
			body:    requesterBody(fmt.Sprintf("Your request \"%s\" was rejected.", title), reason, "Reason", link)})
	case Reopened:
		msgs = append(msgs,
			waitingOn(in.Approvers, title, link, "workflow.resubmitted",
				"Resubmitted for approval: "+title, "Changes were requested and the request is back for a new review."),
			message{recipients: requester, eventType: "workflow.changes_requested",
				subject: "Changes requested: " + title,
				body: requesterBody(fmt.Sprintf("Changes were requested on your request \"%s\". It starts a new review from the first stage.", title),
					reason, "Reason", link)})
	case Escalated:
		msgs = append(msgs,
			waitingOn(in.Approvers, title, link, "workflow.escalated_to_you",
				"Escalated to you: "+title, "This request was escalated to you for a decision."),
			message{recipients: requester, eventType: "workflow.escalated",
				subject: "Escalated: " + title,
				body:    requesterBody(fmt.Sprintf("Your request \"%s\" was escalated.", title), reason, "Reason", link)})
	case Expired:
		msgs = append(msgs, message{recipients: requester, eventType: "workflow.expired",
			subject: "Expired: " + title,
			body: requesterBody(fmt.Sprintf("Your request \"%s\" passed its deadline without a decision. A timeout never counts as approval: start a new request if it is still needed.", title),
				"", "", link)})
	case Cancelled:
		msgs = append(msgs,
			message{recipients: in.Approvers, eventType: "workflow.cancelled",
				subject: "No longer needed: " + title,
				body:    requesterBody(fmt.Sprintf("The request \"%s\" was cancelled. Nothing waits on you.", title), reason, "Reason", link)},
			message{recipients: requester, eventType: "workflow.cancelled",
				subject: "Cancelled: " + title,
				body:    requesterBody(fmt.Sprintf("Your request \"%s\" was cancelled.", title), reason, "Reason", link)})
	case Invalidated:
		msgs = append(msgs,
			message{recipients: in.Approvers, eventType: "workflow.invalidated",
				subject: "No longer needed: " + title,
				body:    requesterBody(fmt.Sprintf("The request \"%s\" was invalidated. Nothing waits on you.", title), reason, "Reason", link)},
			message{recipients: requester, eventType: "workflow.invalidated",
				subject: "Invalidated: " + title,
				body:    requesterBody(fmt.Sprintf("The approval of your request \"%s\" no longer applies.", title), reason, "Reason", link)})
	}

	var out []Notice
	for _, m := range msgs {
		bodies := map[string]string{ChannelInApp: m.body, ChannelEmail: HTMLBody(m.body, link)}
		for _, to := range recipients(m.recipients, in.Actor) {
			for _, ch := range []string{ChannelInApp, ChannelEmail} {
				out = append(out, Notice{
					RecipientPrincipalID: to,
					Channel:              ch,
					Subject:              m.subject,
					Body:                 bodies[ch],
					SourceEventType:      m.eventType,
					SourceReference:      in.WorkflowInstanceID,
				})
			}
		}
	}
	return out
}

// HTMLBody is the EMAIL form of a plain body. notification-svc sends a
// free-text body as text/html exactly as given, so the email copy must be
// HTML: every part is escaped (a reason typed by an approver can never become
// markup or a link in the requester's mailbox), paragraphs and line breaks are
// kept, and the workflow link becomes an anchor. The IN_APP copy stays plain
// text; the console renders it as text.
func HTMLBody(plain, link string) string {
	escapedLink := html.EscapeString(link)
	anchor := `<a href="` + escapedLink + `">` + escapedLink + `</a>`
	var b strings.Builder
	for _, p := range strings.Split(plain, "\n\n") {
		e := strings.ReplaceAll(html.EscapeString(p), "\n", "<br>")
		if link != "" && strings.HasSuffix(e, escapedLink) {
			e = strings.TrimSuffix(e, escapedLink) + anchor
		}
		b.WriteString("<p>" + e + "</p>\n")
	}
	return b.String()
}

// waitingOn is the message to approvers that something waits on them.
func waitingOn(to []string, title, link, eventType, subject, lead string) message {
	return message{recipients: to, eventType: eventType, subject: subject,
		body: lead + "\n\nRequest: " + title + "\n\nDecide in Workflows & Approvals, tab \"My Approvals\": " + link}
}

// requesterBody is a lead sentence, the person's reason if they gave one, and
// where to read the request.
func requesterBody(lead, reason, reasonLabel, link string) string {
	b := lead
	if reason != "" && reasonLabel != "" {
		b += "\n\n" + reasonLabel + ": " + reason
	}
	return b + "\n\nSee the request in Workflows & Approvals: " + link
}

// recipients drops the actor, service principals and repeats, keeping order.
func recipients(in []string, actor string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" || p == actor || seen[p] || IsServicePrincipal(p) {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// IsServicePrincipal reports whether p is a workload rather than a person:
// the platform's svc-* principals and system actors such as the timer.
func IsServicePrincipal(p string) bool {
	return strings.HasPrefix(p, "svc-") || strings.HasPrefix(p, "system:") || p == "system"
}

// Title is how a notice names the request: the workflow type in words, and
// the record it concerns when there is one.
func Title(workflowType, subjectType, subjectID string) string {
	t := Humanize(workflowType)
	if t == "" {
		t = "Approval request"
	}
	if subjectID != "" {
		ref := subjectID
		if len(ref) > 8 {
			ref = ref[:8]
		}
		if st := Humanize(subjectType); st != "" {
			return fmt.Sprintf("%s (%s %s)", t, strings.ToLower(st), ref)
		}
		return fmt.Sprintf("%s (%s)", t, ref)
	}
	return t
}

// Humanize turns a code such as JURISDICTION_RULE_ACTIVATION into
// "Jurisdiction rule activation".
func Humanize(code string) string {
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(strings.TrimSpace(code)))
	if len(words) == 0 {
		return ""
	}
	for i, w := range words {
		words[i] = strings.ToLower(w)
	}
	first := []rune(words[0])
	words[0] = strings.ToUpper(string(first[:1])) + string(first[1:])
	return strings.Join(words, " ")
}

// Link opens the workflow's details on the Workflows & Approvals page.
func Link(baseURL, workflowInstanceID string) string {
	return strings.TrimRight(baseURL, "/") + "/admin/workflows?id=" + workflowInstanceID
}

// clip cuts s to n characters (not bytes, so a multi-byte character is never
// split).
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
