package auth

import "testing"

func TestPINHashRoundTrip(t *testing.T) {
    hash, salt, err := HashPIN("123456")
    if err != nil { t.Fatal(err) }
    if !VerifyPIN("123456", hash, salt) { t.Fatal("correct PIN did not verify") }
    if VerifyPIN("123457", hash, salt) { t.Fatal("wrong PIN verified") }
    if !stringsHasPrefix(hash, "argon2id$") { t.Fatal("new PIN was not Argon2id encoded") }
}

func stringsHasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
