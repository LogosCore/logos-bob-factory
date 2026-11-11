package main

import "github.com/logoscore/logos-golang-protocol/protocol"

// testChannels are the connection schemas of the channels BOB's minions
// speak, as core has them when it makes a build's minion config: the HTTP
// channel's version 3 and the TCP channel's version 1, their fields (the
// TCP channel's targets' too) with the numbers the channels give them (each
// channel's own tests pin them; minion/fields.go reads them). Labels and
// limits are left out: a config needs a field's number and type alone.
func testChannels(module string, version int) (protocol.ConnectionSchema, bool) {
	f := func(key string, num int, typ protocol.FieldType, def any) protocol.FieldSpec {
		return protocol.FieldSpec{Key: key, Num: num, Type: typ, Default: def}
	}
	switch {
	case module == "http" && version == 3:
		address := []protocol.FieldSpec{
			f("base_url", 1, protocol.FieldURL, nil), f("extra_base_urls", 2, protocol.FieldStringList, nil),
			f("host_header", 3, protocol.FieldString, nil), f("sni", 4, protocol.FieldString, nil),
			f("proxy", 5, protocol.FieldURL, nil), f("tls_verify", 6, protocol.FieldBool, false),
			f("tls_pin_sha256", 7, protocol.FieldString, nil), f("user_agent", 8, protocol.FieldString, nil),
			f("headers", 9, protocol.FieldStringMap, nil), f("request_timeout", 10, protocol.FieldDuration, "40s"),
			f("checkin_interval", 11, protocol.FieldDuration, "60s"), f("checkin_jitter", 12, protocol.FieldInt, 20),
		}
		return protocol.ConnectionSchema{SchemaVersion: protocol.ConnectionSchemaV2, Version: 3, Kinds: map[string]protocol.ConnectionKind{
			protocol.KindReverseCall: {Fields: address},
			protocol.KindReversePersistent: {Fields: append(append([]protocol.FieldSpec{}, address...),
				f("ws_reconnect_backoff", 13, protocol.FieldDuration, "60s"))},
		}}, true
	case module == "tcp" && version == 1:
		dial := []protocol.FieldSpec{
			f("host", 1, protocol.FieldString, nil), f("port", 2, protocol.FieldInt, nil),
			f("connect_timeout", 3, protocol.FieldDuration, "10s"),
		}
		checkins := []protocol.FieldSpec{f("checkin_interval", 7, protocol.FieldDuration, "60s"), f("checkin_jitter", 8, protocol.FieldInt, 20)}
		keepalive := f("keepalive", 4, protocol.FieldDuration, "30s")
		return protocol.ConnectionSchema{SchemaVersion: protocol.ConnectionSchemaV2, Version: 1, Kinds: map[string]protocol.ConnectionKind{
			protocol.KindReversePersistent: {Fields: append(append(append([]protocol.FieldSpec{}, dial...), keepalive,
				f("reconnect_backoff", 5, protocol.FieldDuration, "60s")), checkins...)},
			protocol.KindReverseCall: {Fields: append(append(append([]protocol.FieldSpec{}, dial...),
				f("request_timeout", 6, protocol.FieldDuration, "40s")), checkins...)},
			protocol.KindBindPersistent: {Fields: []protocol.FieldSpec{
				f("listen_port", 9, protocol.FieldInt, nil), f("listen_address", 10, protocol.FieldString, "0.0.0.0"),
				f("max_connections", 11, protocol.FieldInt, 1), keepalive,
			}, Target: []protocol.FieldSpec{
				f("host", 1, protocol.FieldString, nil), f("port", 2, protocol.FieldInt, nil),
				f("proxy", 12, protocol.FieldURL, nil), f("connect_timeout", 3, protocol.FieldDuration, "10s"),
			}},
		}}, true
	}
	return protocol.ConnectionSchema{}, false
}

// withConfig is req with the minion config core would make of it.
func withConfig(req protocol.BuildRequest) (protocol.BuildRequest, error) {
	req.MinionConfig = nil
	cfg, err := protocol.EncodeMinionConfig(buildSchema, req, testChannels)
	req.MinionConfig = cfg
	return req, err
}
