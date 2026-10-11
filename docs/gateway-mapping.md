# Gateway HTTP-operation to Janus RPC mapping

This mapping was derived from the generated Lifejacket client operation methods and
current gateway source; Janus and gateway were read only. The client contributes
1863 typed operation names (the generated `WithBody` helpers are excluded), and
the gateway's pinned `janus-client` `JanusServer` interface contributes 160 RPCs.

## Gateway wiring

The gateway exposes one `Server` method per API operation (with generated `Post`
client variants sharing the same server handler). The handler translates the API
request, calls `s.gateway.janus2.<RPC>`, and translates the response. Examples:

- `AccountsCreate -> CreateAccount`: `gateway/accounts.go:185`
- `AccountsGet -> GetAccount`: `gateway/accounts.go:212`
- `DevUsersList` and `DevUsersListPost -> ListDevUsers`: `gateway/dev_users2.go:96`
- `RevOrgsCreate -> CreateRevOrg`: `gateway/rev_orgs2.go:323`
- `ServiceAccountsCreate -> CreateSvcAcc`: `gateway/service_accounts.go:46`

I matched each operation to a Janus RPC only when its gateway handler contained a
single direct call to a method in the JanusServer interface. Handlers with multiple
Janus calls (for example conditional/aggregate paths) are in `unresolved`; all
remaining client operations are also listed there rather than guessed.

## Results

- Operations found: **1863**
- Janus RPCs: **160**
- Mapped: **155**
- Unresolved: **1708**

Naming is not uniformly mechanical: create/get/update/delete commonly use a
singular Janus resource (`AccountsCreate -> CreateAccount`), while list/count/group
usually retain plural resources (`DevUsersList -> ListDevUsers`). Export and sample
CSV operations can call list or bulk-import RPCs, so their mappings follow the
actual gateway call rather than a verb/resource heuristic.
