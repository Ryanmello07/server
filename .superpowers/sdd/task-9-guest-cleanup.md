# Task 9: Guest mode cleanup in remaining files

## Files to modify
- router/handler_utils.go — Remove WrapRequireAuthNoGuest and WrapWithInputRequireAuthNoGuest
- model/peer_model.go — Remove guest mode gating (around line 1034: if session.ByJwt.GuestMode {...})
- model/network_client_model.go — Remove guest mode gating (around line 200: if session.ByJwt.GuestMode || ...)
- model/device_association_model.go — Replace AuthTypeGuest check with direct false (around line 892: isGuestMode := false)
- session/client_session.go — Remove guest mode comment
- jwt/by_jwt.go — Add deprecation comment on GuestMode field: "Deprecated: always false for new tokens. Field kept for backward compat with existing guest JWTs."

## Details

### router/handler_utils.go
Delete the functions `WrapRequireAuthNoGuest` and `WrapWithInputRequireAuthNoGuest` (entire function bodies). These are around lines 112-218 and 284-330ish.

### model/peer_model.go
Around line 1034, remove the guest mode block:
```go
if session.ByJwt.GuestMode {
    // ... return error or similar
}
```

### model/network_client_model.go
Around line 200, remove the GuestMode check. The line `if session.ByJwt.GuestMode || session.ByJwt.ClientId != nil {` should become `if session.ByJwt.ClientId != nil {`

### model/device_association_model.go
Around line 892, change `isGuestMode := (authType == AuthTypeGuest)` to `isGuestMode := false`

### session/client_session.go
Remove the guest mode comment around line 98-103.

### jwt/by_jwt.go
Add a comment above the GuestMode field (line 163):
`// Deprecated: always false for new tokens. Field kept for backward compat with existing guest JWTs.`

## Verification
cd /root/urnetwork/server && go build ./...
