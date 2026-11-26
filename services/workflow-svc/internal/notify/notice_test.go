package notify

import (
	"sort"
	"strings"
	"testing"
)

const (
	wf        = "11111111-2222-4333-8444-555555555555"
	requester = "requester-1"
	approverA = "approver-a"
	approverB = "approver-b"
)

func base(m Moment) Input {
	return Input{
		Moment: m, WorkflowInstanceID: wf, WorkflowType: "JURISDICTION_RULE_ACTIVATION",
		SubjectType: "jurisdiction_rule", SubjectID: "abcdef0123456789", InitiatedBy: requester,
		BaseURL: "https://console.example/",
	}
}

// who returns "recipient/eventType" per notice, one entry per recipient (the
// two channels collapsed), sorted.
func who(ns []Notice) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range ns {
		k := n.RecipientPrincipalID + "/" + n.SourceEventType
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("recipients:\n got  %v\n want %v", got, want)
	}
}

func TestBuild_WhoIsToldWhat(t *testing.T) {
	cases := []struct {
		name string
		in   func() Input
		want []string
	}{
		{"started tells stage-1 approvers only", func() Input {
			in := base(Started)
			in.Actor, in.Approvers = requester, []string{approverA}
			return in
		}, []string{approverA + "/workflow.approval_requested"}},
		{"stage approved tells the next approver and the requester", func() Input {
			in := base(StageApproved)
			in.Actor, in.StageOrder, in.Approvers = approverA, 1, []string{approverB}
			return in
		}, []string{approverB + "/workflow.approval_requested", requester + "/workflow.stage_approved"}},
		{"final approval tells the requester", func() Input {
			in := base(Approved)
			in.Actor = approverB
			return in
		}, []string{requester + "/workflow.approved"}},
		{"rejection tells the requester", func() Input {
			in := base(Rejected)
			in.Actor, in.Rationale = approverA, "missing evidence"
			return in
		}, []string{requester + "/workflow.rejected"}},
		{"reopen tells stage-1 approvers and the requester", func() Input {
			in := base(Reopened)
			in.Actor, in.Approvers = "admin-1", []string{approverA}
			return in
		}, []string{approverA + "/workflow.resubmitted", requester + "/workflow.changes_requested"}},
		{"escalation tells the target and the requester", func() Input {
			in := base(Escalated)
			in.Actor, in.Approvers = "system:workflow-svc", []string{"manager-1"}
			return in
		}, []string{"manager-1/workflow.escalated_to_you", requester + "/workflow.escalated"}},
		{"expiry tells the requester only", func() Input {
			in := base(Expired)
			in.Actor, in.Approvers = "system:workflow-svc", []string{approverA}
			return in
		}, []string{requester + "/workflow.expired"}},
		{"cancellation by an admin tells the waiting approver and the requester", func() Input {
			in := base(Cancelled)
			in.Actor, in.Approvers = "admin-1", []string{approverA}
			return in
		}, []string{approverA + "/workflow.cancelled", requester + "/workflow.cancelled"}},
		{"withdrawal by the requester tells the waiting approver only", func() Input {
			in := base(Cancelled)
			in.Actor, in.Approvers = requester, []string{approverA}
			return in
		}, []string{approverA + "/workflow.cancelled"}},
		{"invalidation tells the waiting approver and the requester", func() Input {
			in := base(Invalidated)
			in.Actor, in.Approvers = "subject-owner", []string{approverA}
			return in
		}, []string{approverA + "/workflow.invalidated", requester + "/workflow.invalidated"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eq(t, who(Build(tc.in())), tc.want)
		})
	}
}

func TestBuild_EveryRecipientGetsInAppAndEmail(t *testing.T) {
	in := base(StageApproved)
	in.Actor, in.StageOrder, in.Approvers = approverA, 1, []string{approverB}
	ns := Build(in)
	if len(ns) != 4 {
		t.Fatalf("2 recipients x 2 channels = 4 notices, got %d", len(ns))
	}
	channels := map[string][]string{}
	for _, n := range ns {
		channels[n.RecipientPrincipalID] = append(channels[n.RecipientPrincipalID], n.Channel)
		if n.SourceReference != wf {
			t.Fatalf("source_reference must be the workflow id, got %q", n.SourceReference)
		}
	}
	for p, chs := range channels {
		sort.Strings(chs)
		if strings.Join(chs, ",") != ChannelEmail+","+ChannelInApp {
			t.Fatalf("%s got channels %v", p, chs)
		}
	}
}

func TestBuild_NeverNotifiesTheActorServicesOrTwice(t *testing.T) {
	in := base(Started)
	in.Actor = approverA
	in.Approvers = []string{approverA, approverB, approverB, "svc-policy", "system:workflow-svc", " ", ""}
	eq(t, who(Build(in)), []string{approverB + "/workflow.approval_requested"})
	if n := len(Build(in)); n != 2 {
		t.Fatalf("one recipient x 2 channels, got %d notices", n)
	}
}

func TestBuild_RejectionCarriesTheReasonAndTheLink(t *testing.T) {
	in := base(Rejected)
	in.Actor, in.Rationale = approverA, "  The evidence pack is missing the 2025 filing.  "
	n := Build(in)[0]
	if n.Subject != "Rejected: Jurisdiction rule activation (jurisdiction rule abcdef01)" {
		t.Fatalf("subject: %q", n.Subject)
	}
	if !strings.Contains(n.Body, "Reason: The evidence pack is missing the 2025 filing.") {
		t.Fatalf("body must quote the reason: %q", n.Body)
	}
	if !strings.Contains(n.Body, "https://console.example/admin/workflows?id="+wf) {
		t.Fatalf("body must link to the workflow: %q", n.Body)
	}
}

func TestBuild_NoReasonNoReasonLine(t *testing.T) {
	in := base(Approved)
	in.Actor = approverB
	if b := Build(in)[0].Body; strings.Contains(b, "Comment:") || strings.Contains(b, "Reason:") {
		t.Fatalf("an approval without a comment has no comment line: %q", b)
	}
}

func TestBuild_LongReasonIsClippedOnACharacter(t *testing.T) {
	in := base(Rejected)
	in.Actor, in.Rationale = approverA, strings.Repeat("é", maxReason+50)
	b := Build(in)[0].Body
	if !strings.Contains(b, strings.Repeat("é", maxReason)+"…") || strings.Contains(b, strings.Repeat("é", maxReason+1)) {
		t.Fatal("the reason must be clipped to maxReason characters")
	}
	if !strings.Contains(b, "\n\nSee the request") {
		t.Fatal("the link survives a long reason")
	}
}

func TestBuild_EscalationWithoutTargetTellsTheRequester(t *testing.T) {
	in := base(Escalated)
	in.Actor = "system:workflow-svc"
	eq(t, who(Build(in)), []string{requester + "/workflow.escalated"})
}

func TestTitleHumanizeLink(t *testing.T) {
	if got := Title("PURCHASE_APPROVAL", "", ""); got != "Purchase approval" {
		t.Fatalf("Title: %q", got)
	}
	if got := Title("", "", "abc"); got != "Approval request (abc)" {
		t.Fatalf("Title without a type: %q", got)
	}
	if got := Humanize("workflow.approval_requested"); got != "Workflow approval requested" {
		t.Fatalf("Humanize: %q", got)
	}
	if got := Link("", wf); got != "/admin/workflows?id="+wf {
		t.Fatalf("relative Link: %q", got)
	}
	if !IsServicePrincipal("svc-workflow") || !IsServicePrincipal("system:workflow-svc") || IsServicePrincipal("33333333-3333-3333-3333-333333333333") {
		t.Fatal("IsServicePrincipal")
	}
}

// notification-svc sends a body as text/html as given: the EMAIL copy must
// escape everything a person typed, keep the paragraphs and link the request;
// the IN_APP copy stays the plain text the console shows.
func TestBuild_EmailBodyIsEscapedHTML(t *testing.T) {
	in := base(Rejected)
	in.Actor = approverA
	in.Rationale = `<a href="https://evil.example">click</a> & <script>alert(1)</script>`
	byChannel := map[string]string{}
	for _, n := range Build(in) {
		byChannel[n.Channel] = n.Body
	}
	plain, email := byChannel[ChannelInApp], byChannel[ChannelEmail]
	if !strings.Contains(plain, "Reason: "+in.Rationale) {
		t.Fatalf("IN_APP keeps the reason as typed: %q", plain)
	}
	if strings.Contains(email, "<script>") || strings.Contains(email, `<a href="https://evil.example">`) {
		t.Fatalf("EMAIL must escape the reason: %q", email)
	}
	if !strings.Contains(email, "&lt;script&gt;alert(1)&lt;/script&gt;") || !strings.Contains(email, "&amp;") {
		t.Fatalf("EMAIL shows the reason escaped: %q", email)
	}
	link := "https://console.example/admin/workflows?id=" + wf
	if !strings.Contains(email, `<a href="`+link+`">`+link+`</a></p>`) {
		t.Fatalf("EMAIL links the request: %q", email)
	}
	if strings.Count(email, "<p>") != 3 {
		t.Fatalf("EMAIL keeps the three paragraphs (lead, reason, link): %q", email)
	}
}

func TestHTMLBody_LineBreaksAndNoLink(t *testing.T) {
	if got := HTMLBody("a\nb\n\nc", ""); got != "<p>a<br>b</p>\n<p>c</p>\n" {
		t.Fatalf("HTMLBody: %q", got)
	}
}
