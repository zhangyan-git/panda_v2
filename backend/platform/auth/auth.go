package auth

type Identity struct{ Subject, Tenant, Roles []string }
type Authorizer interface {
	Authorize(Identity, string, string) error
}
