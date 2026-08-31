package gocommerce

import (
	"context"
	"strings"
	"testing"
)

// The team is the one part of this engine where a bug locks everybody out
// rather than costing money. These tests are mostly about the two ways that can
// happen — nobody left who can grant rights, and a credential that outlives the
// person's access — plus the reason invitations exist at all: nobody but the
// invitee should ever know their password.

func invite(t *testing.T, app *App, email, role string, by *int64) *Invitation {
	t.Helper()
	inv, err := app.Team().Invite(context.Background(), email, role, by)
	if err != nil {
		t.Fatalf("invite %s: %v", email, err)
	}
	return inv
}

func TestAnInvitationIsTheOnlyTimeTheTokenExists(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	inv := invite(t, app, "New.Person@Example.com ", RoleManager, nil)
	if inv.Token == "" {
		t.Fatal("the creating response carried no token; there is no link to send")
	}
	if inv.Email != "new.person@example.com" {
		t.Errorf("email = %q, want it normalised", inv.Email)
	}
	if inv.Status != InvitationPending {
		t.Errorf("status = %q, want pending", inv.Status)
	}
	// The role's rights travel with it, so the person sending the link can see
	// what they are handing over.
	if len(inv.Rights) != len(DefaultRightsOf(RoleManager)) {
		t.Errorf("rights = %v, want the manager's", inv.Rights)
	}

	// Every later read must be unable to produce it, because the store only
	// kept the hash.
	again, err := app.Team().Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if again.Token != "" {
		t.Error("reading an invitation back produced its token; only the hash is stored")
	}
	list, err := app.Team().List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, l := range list {
		if l.Token != "" {
			t.Errorf("listing produced a token for %s", l.Email)
		}
	}
}

func TestAcceptingAnInvitationMakesAnOperatorAndSignsThemIn(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	owner, err := app.Superusers().Create(ctx, "owner@example.com", "correct-horse-battery", RoleOwner)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	inv := invite(t, app, "joiner@example.com", RoleStaff, &owner.ID)

	// Before accepting, the link says who it is for — the accept page has to be
	// able to show that without an account existing yet.
	looked, err := app.Team().Lookup(ctx, inv.Token)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if looked.Email != "joiner@example.com" || looked.Role != RoleStaff {
		t.Errorf("lookup returned %s/%s", looked.Email, looked.Role)
	}

	su, sess, err := app.Team().Accept(ctx, inv.Token, "a-password-they-chose")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if su.Role != RoleStaff {
		t.Errorf("role = %q, want the invited one", su.Role)
	}
	if sess == nil || sess.Token == "" {
		t.Fatal("accepting did not sign the new operator in")
	}
	// The session works, which is what makes signing them in worth anything.
	if _, ok := app.Superusers().Resolve(ctx, sess.Token); !ok {
		t.Error("the session handed back does not resolve")
	}
	// And the password they chose is the one that works.
	if _, _, err := app.Superusers().Authenticate(ctx,
		"joiner@example.com", "a-password-they-chose", "127.0.0.1"); err != nil {
		t.Errorf("signing in with the chosen password: %v", err)
	}

	// The invitation is now history, and says who let them in.
	after, err := app.Team().Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status != InvitationAccepted || after.AcceptedAt == nil {
		t.Errorf("status = %q, accepted_at = %v", after.Status, after.AcceptedAt)
	}
	if after.InvitedByName != "owner@example.com" {
		t.Errorf("invited_by = %q, want the owner", after.InvitedByName)
	}
}

func TestAnInvitationWorksOnce(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	inv := invite(t, app, "twice@example.com", RoleStaff, nil)
	if _, _, err := app.Team().Accept(ctx, inv.Token, "a-password-they-chose"); err != nil {
		t.Fatalf("first accept: %v", err)
	}
	if _, _, err := app.Team().Accept(ctx, inv.Token, "another-password"); err == nil {
		t.Fatal("the same link created a second operator")
	}
	// And a used link no longer says who it was for.
	if _, err := app.Team().Lookup(ctx, inv.Token); err == nil {
		t.Error("a used link still resolves")
	}
}

func TestReinvitingReplacesTheOldLink(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// An owner who resends an invitation means the previous link to stop
	// working. Two live links for one person is exactly what the index prevents,
	// and it must not surface as a unique-violation error either.
	first := invite(t, app, "resend@example.com", RoleStaff, nil)
	second := invite(t, app, "resend@example.com", RoleManager, nil)

	if _, err := app.Team().Lookup(ctx, first.Token); err == nil {
		t.Error("the superseded link still works")
	}
	looked, err := app.Team().Lookup(ctx, second.Token)
	if err != nil {
		t.Fatalf("the new link does not work: %v", err)
	}
	if looked.Role != RoleManager {
		t.Errorf("role = %q, want the re-invited one", looked.Role)
	}
}

func TestInvitingSomebodyAlreadyOnTheTeamIsRefused(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	if _, err := app.Superusers().Create(ctx,
		"here@example.com", "correct-horse-battery", RoleStaff); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := app.Team().Invite(ctx, "here@example.com", RoleOwner, nil)
	if err == nil {
		t.Fatal("invited somebody who is already an operator")
	}
	if !strings.Contains(err.Error(), "change their role") {
		t.Errorf("the error does not point at the right endpoint: %v", err)
	}
}

func TestRevokingClosesTheLink(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	inv := invite(t, app, "regret@example.com", RoleOwner, nil)
	if err := app.Team().Revoke(ctx, inv.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := app.Team().Lookup(ctx, inv.Token); err == nil {
		t.Error("a revoked link still works")
	}
	if _, _, err := app.Team().Accept(ctx, inv.Token, "a-password-they-chose"); err == nil {
		t.Error("a revoked link still creates an operator")
	}

	// An accepted invitation is history and cannot be revoked; removing the
	// operator is the way to undo it, and the message says so.
	other := invite(t, app, "stayed@example.com", RoleStaff, nil)
	if _, _, err := app.Team().Accept(ctx, other.Token, "a-password-they-chose"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	err := app.Team().Revoke(ctx, other.ID)
	if err == nil {
		t.Fatal("revoked an invitation that was already accepted")
	}
	if !strings.Contains(err.Error(), "remove the operator") {
		t.Errorf("the error does not say what to do instead: %v", err)
	}
}

func TestARubbishTokenIsJustNotFound(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// Nothing here should distinguish "never existed" from anything else: the
	// token is the credential and guessing at it must learn nothing.
	if _, err := app.Team().Lookup(ctx, "not-a-real-token"); err == nil {
		t.Error("a made-up token resolved")
	}
	if _, _, err := app.Team().Accept(ctx, "not-a-real-token", "a-password-they-chose"); err == nil {
		t.Error("a made-up token created an operator")
	}
}

// ------------------------------------------------------------- the lockouts

func TestTheLastOwnerCannotBeDeleted(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// The lockout SetRole already refuses, reached through the delete door.
	// Removing the only owner leaves a team where nobody carries settings.write,
	// so nobody can promote anybody — including themselves.
	owner, err := app.Superusers().Create(ctx, "owner@example.com", "correct-horse-battery", RoleOwner)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if _, err := app.Superusers().Create(ctx,
		"staff@example.com", "correct-horse-battery", RoleStaff); err != nil {
		t.Fatalf("create staff: %v", err)
	}

	if err := app.Superusers().Delete(ctx, owner.ID); err == nil {
		t.Fatal("deleted the only owner, leaving a team that cannot grant rights")
	}

	// Promoting somebody else first is the way out, and it works.
	list, err := app.Superusers().List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, su := range list {
		if su.Role == RoleStaff {
			if _, err := app.Superusers().SetRole(ctx, su.ID, RoleOwner); err != nil {
				t.Fatalf("promote: %v", err)
			}
		}
	}
	if err := app.Superusers().Delete(ctx, owner.ID); err != nil {
		t.Errorf("deleting an owner once there are two: %v", err)
	}
}

// ------------------------------------------------------------ self-service

func TestAnOperatorCanChangeTheirOwnPasswordWithoutSettingsWrite(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// A staff member is deliberately denied settings.write. If that were also
	// the right needed to change your own password, somebody suspecting theirs
	// was known would have to ask an owner to choose a new one for them — which
	// is exactly the practice invitations exist to end.
	staff, err := app.Superusers().Create(ctx,
		"staff@example.com", "correct-horse-battery", RoleStaff)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if DefaultCan(RoleStaff, RightSettingsWrite) {
		t.Fatal("staff carries settings.write; this test is checking the wrong thing")
	}

	_, sess, err := app.Superusers().Authenticate(ctx,
		"staff@example.com", "correct-horse-battery", "127.0.0.1")
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}

	// The current password is required: a session left open on an unlocked
	// laptop must not be enough to take the account over.
	if _, err := app.Superusers().UpdateSelf(ctx, staff.ID,
		"the-wrong-one", "", "a-brand-new-password", sess.Token); err == nil {
		t.Error("changed a password without proving the current one")
	}

	if _, err := app.Superusers().UpdateSelf(ctx, staff.ID,
		"correct-horse-battery", "", "a-brand-new-password", sess.Token); err != nil {
		t.Fatalf("change own password: %v", err)
	}
	if _, _, err := app.Superusers().Authenticate(ctx,
		"staff@example.com", "a-brand-new-password", "127.0.0.1"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}

func TestChangingYourPasswordKeepsThisBrowserAndDropsTheRest(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	su, err := app.Superusers().Create(ctx, "two@example.com", "correct-horse-battery", RoleOwner)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, here, err := app.Superusers().Authenticate(ctx, "two@example.com", "correct-horse-battery", "127.0.0.1")
	if err != nil {
		t.Fatalf("sign in here: %v", err)
	}
	_, elsewhere, err := app.Superusers().Authenticate(ctx, "two@example.com", "correct-horse-battery", "127.0.0.2")
	if err != nil {
		t.Fatalf("sign in elsewhere: %v", err)
	}

	if _, err := app.Superusers().UpdateSelf(ctx, su.ID,
		"correct-horse-battery", "", "a-brand-new-password", here.Token); err != nil {
		t.Fatalf("change password: %v", err)
	}

	if _, ok := app.Superusers().Resolve(ctx, here.Token); !ok {
		t.Error("changing your own password signed you out of the browser you did it in")
	}
	if _, ok := app.Superusers().Resolve(ctx, elsewhere.Token); ok {
		t.Error("the other session survived a password change; that is the point of the change")
	}
}

func TestSigningSomebodyOutEverywhereEndsEverySession(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// What an owner reaches for when a laptop goes missing, or before removing
	// somebody. A role change takes effect on the next request because Resolve
	// re-reads it, but a session has to be ended to stop it.
	su, err := app.Superusers().Create(ctx, "gone@example.com", "correct-horse-battery", RoleManager)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var tokens []string
	for _, ip := range []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"} {
		_, sess, err := app.Superusers().Authenticate(ctx, "gone@example.com", "correct-horse-battery", ip)
		if err != nil {
			t.Fatalf("sign in from %s: %v", ip, err)
		}
		tokens = append(tokens, sess.Token)
	}

	n, newest, err := app.Superusers().Sessions(ctx, su.ID)
	if err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if n != 3 || newest == nil {
		t.Errorf("sessions = %d, newest = %v, want 3 and a time", n, newest)
	}

	revoked, err := app.Superusers().RevokeAll(ctx, su.ID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if revoked != 3 {
		t.Errorf("revoked = %d, want 3", revoked)
	}
	for i, tok := range tokens {
		if _, ok := app.Superusers().Resolve(ctx, tok); ok {
			t.Errorf("session %d still resolves after signing out everywhere", i)
		}
	}

	// An id nobody has is a 404, not a quiet nought.
	if _, err := app.Superusers().RevokeAll(ctx, 424242); err == nil {
		t.Error("revoking sessions for a superuser that does not exist reported success")
	}
}

func TestDemotingSomebodyTakesEffectOnTheirOpenSession(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// A role lives on the operator row, and Resolve reads it per request — so a
	// demotion must not wait for the session to expire. This is the property
	// that makes "change the role" a real control rather than a label.
	if _, err := app.Superusers().Create(ctx,
		"boss@example.com", "correct-horse-battery", RoleOwner); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	victim, err := app.Superusers().Create(ctx,
		"demoted@example.com", "correct-horse-battery", RoleOwner)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, sess, err := app.Superusers().Authenticate(ctx,
		"demoted@example.com", "correct-horse-battery", "127.0.0.1")
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}

	if _, err := app.Superusers().SetRole(ctx, victim.ID, RoleStaff); err != nil {
		t.Fatalf("demote: %v", err)
	}

	resolved, ok := app.Superusers().Resolve(ctx, sess.Token)
	if !ok {
		t.Fatal("the session stopped resolving; demotion is not removal")
	}
	if resolved.Role != RoleStaff {
		t.Errorf("role on the open session = %q, want staff", resolved.Role)
	}
	if DefaultCan(resolved.Role, RightSettingsWrite) {
		t.Error("the demoted operator still carries settings.write on their open session")
	}
}
