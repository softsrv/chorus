package email

import "fmt"

// VerificationEmail returns subject, HTML, and plain-text bodies for an email verification link.
func VerificationEmail(appName, verifyURL string) (subject, html, text string) {
	subject = fmt.Sprintf("Verify your %s email address", appName)
	text = fmt.Sprintf(
		"Click the link below to verify your email address:\n\n%s\n\nThis link expires in 24 hours.\nIf you did not create an account, you can safely ignore this email.",
		verifyURL,
	)
	html = fmt.Sprintf(`<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;max-width:600px;margin:auto;padding:24px">
  <h2>Verify your email</h2>
  <p>Click the button below to verify your email address. This link expires in <strong>24 hours</strong>.</p>
  <p>
    <a href="%s" style="display:inline-block;background:#4f46e5;color:#fff;padding:12px 24px;border-radius:6px;text-decoration:none;font-weight:bold">
      Verify Email
    </a>
  </p>
  <p style="color:#888;font-size:12px">If you did not create an account, you can safely ignore this email.</p>
</body>
</html>`, verifyURL)
	return
}

// PasswordResetEmail returns subject, HTML, and plain-text bodies for a password reset link.
func PasswordResetEmail(appName, resetURL string) (subject, html, text string) {
	subject = fmt.Sprintf("Reset your %s password", appName)
	text = fmt.Sprintf(
		"Click the link below to reset your password:\n\n%s\n\nThis link expires in 1 hour.\nIf you did not request a password reset, you can safely ignore this email.",
		resetURL,
	)
	html = fmt.Sprintf(`<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;max-width:600px;margin:auto;padding:24px">
  <h2>Reset your password</h2>
  <p>Click the button below to reset your password. This link expires in <strong>1 hour</strong>.</p>
  <p>
    <a href="%s" style="display:inline-block;background:#4f46e5;color:#fff;padding:12px 24px;border-radius:6px;text-decoration:none;font-weight:bold">
      Reset Password
    </a>
  </p>
  <p style="color:#888;font-size:12px">If you did not request a password reset, you can safely ignore this email.</p>
</body>
</html>`, resetURL)
	return
}

// PasswordChangedEmail returns a confirmation email after a successful password change.
func PasswordChangedEmail(appName string) (subject, html, text string) {
	subject = fmt.Sprintf("Your %s password was changed", appName)
	text = "Your password has been successfully changed. If you did not make this change, please contact support immediately."
	html = `<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;max-width:600px;margin:auto;padding:24px">
  <h2>Password changed</h2>
  <p>Your password has been successfully changed.</p>
  <p style="color:#888;font-size:12px">If you did not make this change, please contact support immediately.</p>
</body>
</html>`
	return
}

// AccountExistsEmail is sent when someone tries to register with an address
// that already has an account. Registration responds identically either way so
// the API doesn't reveal which emails are registered; this email tells the real
// owner what happened.
func AccountExistsEmail(appName, resetURL string) (subject, html, text string) {
	subject = fmt.Sprintf("Your %s account", appName)
	text = fmt.Sprintf(
		"Someone tried to create a %s account with this email address, but you already have one.\n\nIf this was you, sign in instead. If you forgot your password, reset it here:\n\n%s\n\nIf this wasn't you, you can safely ignore this email.",
		appName, resetURL,
	)
	html = fmt.Sprintf(`<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;max-width:600px;margin:auto;padding:24px">
  <h2>You already have an account</h2>
  <p>Someone tried to create a %s account with this email address, but you already have one.</p>
  <p>If this was you, sign in instead. If you forgot your password, <a href="%s">reset it here</a>.</p>
  <p style="color:#888;font-size:12px">If this wasn't you, you can safely ignore this email.</p>
</body>
</html>`, appName, resetURL)
	return
}
