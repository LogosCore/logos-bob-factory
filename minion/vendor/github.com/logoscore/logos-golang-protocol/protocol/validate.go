package protocol

import (
	"fmt"
	"regexp"
	"strings"
)

// syncIDPattern is a lowercase UUID, the form of message and build ids.
var syncIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// MaxChannelInstanceLength caps the instance id a sync request names.
const MaxChannelInstanceLength = 128

// ValidateInbound checks a channel's message: its headers
// (ValidateInboundHeaders), and data that is not empty.
func ValidateInbound(msg InboundMinionMessage) error {
	if err := ValidateInboundHeaders(msg); err != nil {
		return err
	}
	if len(msg.Data) == 0 {
		return newValidationError(ErrCodeMissingField, "the body is empty: a minion always sends something")
	}
	return nil
}

// ValidateInboundHeaders checks the header fields of a channel's message,
// so a server can refuse a request before reading its body: both ids are
// lowercase UUIDs and the instance is set.
func ValidateInboundHeaders(msg InboundMinionMessage) error {
	if err := validateSyncIDs(msg.MessageID, msg.BuildID); err != nil {
		return err
	}
	if strings.TrimSpace(msg.ChannelInstance) == "" {
		return newValidationError(ErrCodeMissingField, HeaderChannelInstance+" is required")
	}
	if len(msg.ChannelInstance) > MaxChannelInstanceLength {
		return newValidationError(ErrCodeInvalidValue, fmt.Sprintf("%s is longer than %d characters", HeaderChannelInstance, MaxChannelInstanceLength))
	}
	return nil
}

// ValidateOutbound checks core's reply. Its data may be empty: core has
// nothing for the minion.
func ValidateOutbound(msg OutboundMinionMessage) error {
	return validateSyncIDs(msg.MessageID, msg.BuildID)
}

func validateSyncIDs(messageID, buildID string) error {
	for _, f := range []struct{ name, value string }{{HeaderMessageID, messageID}, {HeaderBuildID, buildID}} {
		if f.value == "" {
			return newValidationError(ErrCodeMissingField, f.name+" is required")
		}
		if !syncIDPattern.MatchString(f.value) {
			return newValidationError(ErrCodeInvalidValue, f.name+" must be a lowercase UUID")
		}
	}
	return nil
}
