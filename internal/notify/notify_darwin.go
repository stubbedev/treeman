package notify

// osSender returns the native sender for macOS: osascript posting to
// Notification Center.
func osSender() Sender { return osascriptSender{} }
