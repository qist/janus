package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"janus/internal/oauth"
)

// DeviceSessionTTL bounds how long a started device sign-in may be polled.
const DeviceSessionTTL = 15 * 60 // seconds

// Device-flow errors mapped to stable API codes.
var (
	ErrDeviceDenied  = errors.New("device authorization denied")
	ErrDeviceExpired = errors.New("device authorization expired")
)

// DeviceStart is what the console shows: a code to type on the vendor page.
type DeviceStart struct {
	SessionID       string `json:"session_id"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int64  `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// DevicePoll is one poll result. Status is "pending" until the operator finishes
// authorizing; then it is "complete" and carries the new credential.
type DevicePoll struct {
	Status     string      `json:"status"`
	Interval   int         `json:"interval,omitempty"`
	Credential *Credential `json:"credential,omitempty"`
}

type deviceSession struct {
	ID         string
	ProviderID string
	DeviceCode string
	Interval   int
	ExpiresAt  int64
	Label      string
}

type deviceSessions struct {
	mu   sync.Mutex
	data map[string]deviceSession
}

// deviceConfig returns the flow registered for this provider's channel. Like
// OAuth, only the channel decides: a kind-based fallback would send unrelated
// providers to GitHub.
func (s *Service) deviceConfig(p Provider) (oauth.DeviceConfig, bool) {
	registry := s.Devices
	if registry == nil {
		registry = oauth.DefaultDeviceRegistry()
	}
	return registry.Get(p.Channel)
}

// StartDevice begins a device authorization. The operator visits
// verification_uri, enters user_code, and the console polls PollDevice.
func (s *Service) StartDevice(ctx context.Context, providerID, label string) (DeviceStart, error) {
	p, err := s.Store.GetProvider(ctx, providerID)
	if err != nil {
		return DeviceStart{}, err
	}
	cfg, ok := s.deviceConfig(p)
	if !ok {
		return DeviceStart{}, fmt.Errorf("%w: %s", ErrOAuthUnsupported, p.ChannelOrKind())
	}

	code, err := cfg.Start(ctx)
	if err != nil {
		return DeviceStart{}, err
	}

	session := deviceSession{
		ID:         "dev_" + strings.TrimPrefix(newOAuthSessionID(), "oauth_"),
		ProviderID: p.ID,
		DeviceCode: code.DeviceCode,
		Interval:   code.Interval,
		ExpiresAt:  s.now() + int64(code.ExpiresIn),
		Label:      strings.TrimSpace(label),
	}

	s.devices.mu.Lock()
	if s.devices.data == nil {
		s.devices.data = make(map[string]deviceSession)
	}
	for id, existing := range s.devices.data {
		if s.now() > existing.ExpiresAt {
			delete(s.devices.data, id)
		}
	}
	s.devices.data[session.ID] = session
	s.devices.mu.Unlock()

	return DeviceStart{
		SessionID:       session.ID,
		UserCode:        code.UserCode,
		VerificationURI: code.VerificationURI,
		ExpiresIn:       int64(code.ExpiresIn),
		Interval:        code.Interval,
	}, nil
}

// PollDevice checks once whether authorization completed. The session survives a
// "pending" answer; it is dropped on success, denial or expiry.
func (s *Service) PollDevice(ctx context.Context, providerID, sessionID string) (DevicePoll, error) {
	s.devices.mu.Lock()
	session, ok := s.devices.data[sessionID]
	s.devices.mu.Unlock()
	if !ok || session.ProviderID != providerID {
		return DevicePoll{}, ErrOAuthSession
	}
	if s.now() > session.ExpiresAt {
		s.dropDeviceSession(sessionID)
		return DevicePoll{}, ErrDeviceExpired
	}

	p, err := s.Store.GetProvider(ctx, providerID)
	if err != nil {
		return DevicePoll{}, err
	}
	cfg, ok := s.deviceConfig(p)
	if !ok {
		s.dropDeviceSession(sessionID)
		return DevicePoll{}, fmt.Errorf("%w: %s", ErrOAuthUnsupported, p.ChannelOrKind())
	}

	tokens, err := cfg.Poll(ctx, session.DeviceCode)
	switch {
	case errors.Is(err, oauth.ErrDevicePending):
		return DevicePoll{Status: "pending", Interval: session.Interval}, nil
	case errors.Is(err, oauth.ErrDeviceSlowDown):
		session.Interval += 5
		if session.Interval > 60 {
			session.Interval = 60
		}
		s.devices.mu.Lock()
		if current, ok := s.devices.data[sessionID]; ok {
			current.Interval = session.Interval
			s.devices.data[sessionID] = current
		}
		s.devices.mu.Unlock()
		return DevicePoll{Status: "pending", Interval: session.Interval}, nil
	case errors.Is(err, oauth.ErrDeviceExpired):
		s.dropDeviceSession(sessionID)
		return DevicePoll{}, ErrDeviceExpired
	case errors.Is(err, oauth.ErrDeviceDenied):
		s.dropDeviceSession(sessionID)
		return DevicePoll{}, ErrDeviceDenied
	case err != nil:
		return DevicePoll{}, err
	}

	// Exchange immediately so a token that cannot actually call the API is a
	// failed sign-in rather than a credential that fails later.
	derived, expiresAt, err := cfg.Exchange(ctx, tokens.AccessToken)
	if err != nil {
		s.dropDeviceSession(sessionID)
		return DevicePoll{}, err
	}
	tokens.DerivedToken = derived
	tokens.DerivedExpiresAt = expiresAt

	account := cfg.Login(ctx, tokens.AccessToken)
	if account == "" {
		account = "github"
	}
	label := session.Label
	if label == "" {
		label = account
	}
	credential, err := s.CreateOAuthCredential(ctx, p.ID, label, account, "", tokens)
	if err != nil {
		return DevicePoll{}, err
	}
	s.dropDeviceSession(sessionID)
	return DevicePoll{Status: "complete", Credential: &credential}, nil
}

func (s *Service) dropDeviceSession(id string) {
	s.devices.mu.Lock()
	delete(s.devices.data, id)
	s.devices.mu.Unlock()
}
