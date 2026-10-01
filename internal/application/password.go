package application

import "golang.org/x/crypto/bcrypt"

const bcryptCost = 12

// dummyHash is a bcrypt hash of a value nobody can supply, computed once at
// init. VerifyDummyPassword compares against it so an unknown account costs
// the same wall-clock time as a wrong password.
//
// Without it, login leaks account existence through latency: the not-found
// path returns in ~1ms while a wrong password spends ~250ms in bcrypt. That
// is a trivially measurable oracle, and it defeats the entire point of
// returning an identical error message for both cases.
var dummyHash, _ = bcrypt.GenerateFromPassword(
	[]byte("golaunch-nonexistent-account-placeholder"), bcryptCost)

// VerifyDummyPassword always reports failure. It exists purely to burn the
// same CPU a real comparison would.
func VerifyDummyPassword(plain string) bool {
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(plain))
	return false
}

func HashPassword(plain string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func VerifyPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
