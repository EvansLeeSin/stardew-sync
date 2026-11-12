package main

import (
	"os"

	// Embedded Mozilla root bundle, used only if no system roots can be loaded.
	_ "golang.org/x/crypto/x509roots/fallback"
)

// androidCADirs are Android's trusted CA directories. The agent is built with
// GOOS=linux (static, no cgo), so Go's crypto/x509 only searches desktop Linux
// paths (/etc/ssl/certs, ...) which do not exist on Android, causing
// "x509: certificate signed by unknown authority". Point it at Android's stores:
//   - APEX conscrypt store (Android 14+, updatable via Play system updates)
//   - legacy system store
//   - user-installed CAs (owner user)
var androidCADirs = "/apex/com.android.conscrypt/cacerts:/system/etc/security/cacerts:/data/misc/user/0/cacerts-added"

// setupCARoots must run before the first TLS connection (crypto/x509 loads
// system roots lazily on first use and reads SSL_CERT_DIR at that time).
func setupCARoots() {
	if os.Getenv("SSL_CERT_DIR") != "" || os.Getenv("SSL_CERT_FILE") != "" {
		return // explicit override
	}
	if _, err := os.Stat("/system/etc/security/cacerts"); err != nil {
		if _, err := os.Stat("/apex/com.android.conscrypt/cacerts"); err != nil {
			return // not Android; keep Go defaults
		}
	}
	os.Setenv("SSL_CERT_DIR", androidCADirs)
}
