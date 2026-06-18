package notify

// osSender returns the native sender for Linux: notify-send (libnotify).
func osSender() Sender { return notifySendSender{} }
