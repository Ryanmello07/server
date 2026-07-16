# Task 7: Controllers (seedphrase, account, auth binding)

## Files to create
- controller/seedphrase_controller.go (NEW)
- controller/account_controller.go (NEW)

## Files to modify
- controller/auth_controller.go (add AddAuth + RemoveAuth controllers)
- controller/network_controller.go (remove UpgradeFromGuest + UpgradeFromGuestExisting controllers)

## What to implement

### 1. controller/seedphrase_controller.go (NEW)
Create with these types and functions:

```go
package controller

import (
    "fmt"
    "github.com/urnetwork/server/model"
    "github.com/urnetwork/server/session"
)

type RegenerateSeedphraseArgs struct{}
type RegenerateSeedphraseResult struct { Seedphrase string `json:"seedphrase"` }
type GenerateSeedphraseArgs struct{}
type GenerateSeedphraseResult struct { Seedphrase string `json:"seedphrase"` }

func RegenerateSeedphrase(args RegenerateSeedphraseArgs, session *session.ClientSession) (*RegenerateSeedphraseResult, error) {
    hasSeedphrase, err := model.HasSeedphraseAuth(session.Ctx, session.ByJwt.UserId)
    if err != nil { return nil, err }
    if !hasSeedphrase { return nil, fmt.Errorf("no seedphrase to regenerate") }
    newSeedphrase, err := model.RegenerateSeedphrase(session.Ctx, session.ByJwt.UserId)
    if err != nil { return nil, err }
    return &RegenerateSeedphraseResult{Seedphrase: newSeedphrase}, nil
}

func GenerateSeedphrase(args GenerateSeedphraseArgs, session *session.ClientSession) (*GenerateSeedphraseResult, error) {
    hasSeedphrase, err := model.HasSeedphraseAuth(session.Ctx, session.ByJwt.UserId)
    if err != nil { return nil, err }
    if hasSeedphrase { return nil, fmt.Errorf("seedphrase already exists, use regenerate instead") }
    newSeedphrase, err := model.GenerateSeedphrase(session.Ctx, session.ByJwt.UserId)
    if err != nil { return nil, err }
    return &GenerateSeedphraseResult{Seedphrase: newSeedphrase}, nil
}
```

### 2. controller/account_controller.go (NEW)
Create with ChangeNetworkName and ClaimNetworkName controllers. Uses model.ValidateNetworkName (exported from network_model.go). Needs imports for context, time, server, model, session, fmt.

Key functions: ChangeNetworkName (checks availability against network + network_name_reclaim, stores old name in reclaim pool with 24h cooldown, cleans expired reclaims). ClaimNetworkName (same but no cooldown).

### 3. controller/auth_controller.go (MODIFY)
Add AddAuth and RemoveAuth controllers at end of file:
- AddAuth wraps model.AddAuth (already exists in model/network_user_model.go)
- RemoveAuth wraps model.RemoveAuth (already exists in model/network_user_model.go)
See existing patterns: controller functions take (args, session) and return (result, error).

### 4. controller/network_controller.go (MODIFY)
Remove UpgradeFromGuest and UpgradeFromGuestExisting functions.

## Model functions available:
- model.HasSeedphraseAuth(ctx, userId) (bool, error)
- model.RegenerateSeedphrase(ctx, userId) (string, error)
- model.GenerateSeedphrase(ctx, userId) (string, error)
- model.AddAuth(args, session) (*AddAuthMethodResult, error)
- model.RemoveAuth(ctx, userId, authType string) error
- model.ValidateNetworkName(name string) (string, error) — exported version of validateNetworkName

## Verification
cd /root/urnetwork/server && go build ./...
