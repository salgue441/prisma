# 07 · Response masking

Upstream services often return more than a given client should see. Masking
rewrites response bodies at the gateway, so sensitive fields are hidden in one
place instead of in every service.

## Run

```sh
export PRISMA_MASK_HASH_KEY=change-me
go run ./cmd/demo-upstream
go run ./cmd/prisma -config examples/07-masking/prisma.yaml
```

## What a support agent sees

```sh
curl -s -H 'X-API-Key: support-key' localhost:8080/orders/42 | jq .
```

```json
{
  "customer": {
    "email": "hmac:3f8aadf3b1f2dac1e6fda94366234c5c",
    "name": "Ana Example",
    "payment": { "cardNumber": "************1111" },
    "ssn": "***"
  },
  "id": "42",
  "instance": "demo-1",
  "seenConsumer": ""
}
```

- `ssn` is redacted, `cardNumber` keeps its last 4 digits, `requestId` is gone.
- `email` is replaced by a keyed hash: the same address always gets the same
  hash, so agents can still correlate records without seeing it. Change
  `hashKey` and every hash changes.

## What compliance sees

The `compliance` consumer has the `pii:read` scope, which `unlessScope` exempts:

```sh
curl -s -H 'X-API-Key: compliance-key' localhost:8080/orders/42 | jq .customer.ssn
# "123-45-6789"
```

## Rules

| Rule | Matches |
|---|---|
| `selector: "$.a.b"` | an exact path |
| `selector: "$..ssn"` | `ssn` at any depth |
| `selector: "$.items[*].card"` | `card` in every element of `items` |
| `detector: email` / `ssn` / `credit_card` | values that look like one (cards are Luhn-checked) |

| Strategy | Result |
|---|---|
| `redact` | `"***"` |
| `partial` | keeps the last `keepLast` characters |
| `hash` | HMAC of the value with `hashKey` |
| `drop` | removes the field |

## Failure behavior

With `onFailure: closed` (the default), a response that should be masked but
can't be (not JSON, larger than `maxBufferBytes`) is replaced by an error
rather than passed through, and `prisma_masking_failures_total` counts it.
Set `onFailure: open` only for data that isn't sensitive.

## GraphQL and gRPC

The same rules apply to GraphQL responses, and to gRPC messages when the
policy has a `descriptorSet` (`protoc --include_imports
--descriptor_set_out=api.pb ...`); see [09-grpc](../09-grpc).
