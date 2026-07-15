# Task 8: API handlers and route registration

## Files to create
- api/handlers/seedphrase_handlers.go (NEW)
- api/handlers/account_handlers.go (NEW — or modify if exists)

## Files to modify
- api/handlers/auth_handlers.go (add AddAuth + RemoveAuth handlers)
- api/handlers/network_user_handlers.go (remove UpgradeGuest + UpgradeGuestExisting handlers)
- api/api.go (remove upgrade-guest routes, add new routes)

## What to implement

### 1. api/handlers/seedphrase_handlers.go (NEW)
```go
package handlers
import (...)
func RegenerateSeedphrase(w http.ResponseWriter, r *http.Request) {
    router.WrapWithInputRequireAuth(controller.RegenerateSeedphrase, w, r)
}
func GenerateSeedphrase(w http.ResponseWriter, r *http.Request) {
    router.WrapWithInputRequireAuth(controller.GenerateSeedphrase, w, r)
}
```

### 2. api/handlers/auth_handlers.go (MODIFY)
Add at end:
- func AddAuth(w, r) { router.WrapWithInputRequireAuth(controller.AddAuth, w, r) }
- func RemoveAuth(w, r) { router.WrapWithInputRequireAuth(controller.RemoveAuth, w, r) }

### 3. api/handlers/account_handlers.go (CREATE if not exists, otherwise MODIFY)
- func ChangeNetworkName(w, r) { router.WrapWithInputRequireAuth(controller.ChangeNetworkName, w, r) }
- func ClaimNetworkName(w, r) { router.WrapWithInputRequireAuth(controller.ClaimNetworkName, w, r) }

### 4. api/handlers/network_user_handlers.go (MODIFY)
Remove: UpgradeGuest and UpgradeGuestExisting handler functions.

### 5. api/api.go (MODIFY)
Remove these routes (around lines 50-51):
```
router.NewRoute("POST", "/auth/upgrade-guest", handlers.UpgradeGuest),
router.NewRoute("POST", "/auth/upgrade-guest-existing", handlers.UpgradeGuestExisting),
```

Add these routes in the auth section:
```
router.NewRoute("POST", "/auth/add-auth", handlers.AddAuth),
router.NewRoute("POST", "/auth/remove-auth", handlers.RemoveAuth),
router.NewRoute("POST", "/auth/regenerate-seedphrase", handlers.RegenerateSeedphrase),
router.NewRoute("POST", "/auth/generate-seedphrase", handlers.GenerateSeedphrase),
```

Add these routes in the account section (after the existing /account/ routes):
```
router.NewRoute("POST", "/account/change-name", handlers.ChangeNetworkName),
router.NewRoute("POST", "/account/claim-name", handlers.ClaimNetworkName),
```

## Verification
cd /root/urnetwork/server && go build ./...
