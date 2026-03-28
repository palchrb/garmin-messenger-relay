// Package fcm provides Android-native FCM (Firebase Cloud Messaging) push
// notification support for the Garmin Messenger (Hermes) protocol.
//
// It implements GCM device registration, FCM token acquisition, and an MCS
// (Mobile Connection Server) client for receiving real-time push notifications
// from Garmin's Hermes backend.
//
// Usage:
//
//	client := fcm.NewClient(sessionDir)
//	client.OnMessage(func(msg fcm.NewMessage) { ... })
//	token, err := client.Register(ctx)
//	err = client.Listen(ctx)
package fcm
