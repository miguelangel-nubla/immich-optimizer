package library

import "testing"

// Two API keys of one user never have two services working on that user's
// assets; the second gets the user once the first lets go.
func TestUsersOneServicePerUser(t *testing.T) {
	u := NewUsers()
	a, b := &Service{}, &Service{}
	if !u.claim("user1", a) {
		t.Fatal("the first service could not take its user")
	}
	if !u.claim("user1", a) {
		t.Fatal("the service could not take its own user again (a reconnect)")
	}
	if u.claim("user1", b) {
		t.Fatal("a second service took a user already taken")
	}
	if !u.claim("user2", b) {
		t.Fatal("another user was refused")
	}
	u.release(a)
	if !u.claim("user1", b) {
		t.Fatal("the user was not free after release")
	}
	var none *Users
	if !none.claim("x", a) {
		t.Fatal("without a shared set every claim succeeds")
	}
	none.release(a)
}
