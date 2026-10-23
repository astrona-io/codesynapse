package accountstest

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// Bootstrap accounts and password used by Bootstrap.
const (
	RootEmail     = "root@bootstrap.test"
	SetupOwner    = "owner@bootstrap.test"
	SetupPassword = "bootstrap root password" //nolint:gosec // test fixture, not a credential

)

// Bootstrap sets up an empty installation the way an operator does: the
// first sign-up becomes the bootstrap root, which hands over to a standard
// owner of a "Setup" organisation. Later sign-ups (with SignupOpen) are
// standard accounts. It returns a fresh root session for governance calls.
func Bootstrap(ctx context.Context, svc *accounts.Service) (accounts.Principal, error) {
	if _, _, err := svc.SignUp(ctx, RootEmail, "Root", SetupPassword); err != nil {
		return accounts.Principal{}, err
	}
	root, err := login(ctx, svc, RootEmail)
	if err != nil {
		return accounts.Principal{}, err
	}
	if _, err := svc.CompleteSetup(ctx, root, accounts.SetupInput{OrgName: "Setup", Email: SetupOwner, Name: "Setup Owner", Password: SetupPassword}); err != nil {
		return accounts.Principal{}, err
	}
	// The handover ended the setup session.
	return login(ctx, svc, RootEmail)
}

func login(ctx context.Context, svc *accounts.Service, email string) (accounts.Principal, error) {
	return loginWith(ctx, svc, email, SetupPassword)
}

func loginWith(ctx context.Context, svc *accounts.Service, email, password string) (accounts.Principal, error) {
	token, _, err := svc.Login(ctx, email, password, "bootstrap")
	if err != nil {
		return accounts.Principal{}, err
	}
	return svc.Authenticate(ctx, token)
}
