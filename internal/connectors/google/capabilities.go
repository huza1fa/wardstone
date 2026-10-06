package google

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"

	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/domain"
)

const Name domain.ConnectorName = "google"

const (
	AccessInspect      domain.CapabilityName = "google.access.inspect"
	UsersGet           domain.CapabilityName = "google.users.get"
	UsersListGroups    domain.CapabilityName = "google.users.list_groups"
	GroupsGet          domain.CapabilityName = "google.groups.get"
	GroupsListMembers  domain.CapabilityName = "google.groups.list_members"
	GroupsAddMember    domain.CapabilityName = "google.groups.add_member"
	GroupsRemoveMember domain.CapabilityName = "google.groups.remove_member"
)

func Capabilities() []capabilities.Definition {
	return []capabilities.Definition{
		{Name: AccessInspect, Connector: Name, Description: "Inspect administrator-configured access request user, group, and direct membership; selectors come only from ticket fields and authenticated requester context", Effect: domain.EffectRead, ArgumentsVersion: 1, Idempotent: true, ValidateArguments: validateAccessInspect},
		{Name: UsersGet, Connector: Name, Description: "Get a Google Workspace user", Effect: domain.EffectRead, ArgumentsVersion: 1, Idempotent: true, ValidateArguments: validateUser},
		{Name: UsersListGroups, Connector: Name, Description: "List groups for a Google Workspace user", Effect: domain.EffectRead, ArgumentsVersion: 1, Idempotent: true, ValidateArguments: validateUser},
		{Name: GroupsGet, Connector: Name, Description: "Get a Google Workspace group", Effect: domain.EffectRead, ArgumentsVersion: 1, Idempotent: true, ValidateArguments: validateGroup},
		{Name: GroupsListMembers, Connector: Name, Description: "List members of a Google Workspace group", Effect: domain.EffectRead, ArgumentsVersion: 1, Idempotent: true, ValidateArguments: validateGroup},
		{Name: GroupsAddMember, Connector: Name, Description: "Add a member to a Google Workspace group", Effect: domain.EffectMutate, ArgumentsVersion: 1, Idempotent: true, ValidateArguments: validateGroupMember},
		{Name: GroupsRemoveMember, Connector: Name, Description: "Remove a member from a Google Workspace group", Effect: domain.EffectMutate, ArgumentsVersion: 1, Idempotent: true, ValidateArguments: validateGroupMember},
	}
}

// The collector owns selectors; this capability accepts no model-supplied keys.
func validateAccessInspect(data json.RawMessage) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("access inspection accepts only an empty argument object")
	}
	return decodeStrict(data, &struct{}{})
}

func validateUser(data json.RawMessage) error {
	var value struct {
		User string `json:"user"`
	}
	if err := decodeStrict(data, &value); err != nil {
		return err
	}
	return validateEmail("user", value.User)
}

func validateGroup(data json.RawMessage) error {
	var value struct {
		Group string `json:"group"`
	}
	if err := decodeStrict(data, &value); err != nil {
		return err
	}
	return validateEmail("group", value.Group)
}

func validateGroupMember(data json.RawMessage) error {
	var value struct {
		Group string `json:"group"`
		User  string `json:"user"`
	}
	if err := decodeStrict(data, &value); err != nil {
		return err
	}
	if err := validateEmail("group", value.Group); err != nil {
		return err
	}
	return validateEmail("user", value.User)
}

func decodeStrict(data json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON object")
	}
	return nil
}

func validateEmail(field, value string) error {
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value {
		return fmt.Errorf("%s must be an email address", field)
	}
	return nil
}
