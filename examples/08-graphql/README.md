# 08 · GraphQL

One GraphQL request can ask for a huge amount of work. The gateway parses
each operation and enforces limits before it reaches the server. The demo
GraphQL upstream returns canned data and echoes the query it received in
`extensions.receivedQuery`, so you can see what the gateway forwarded.

## Run

From the repository root:

```sh
go run ./cmd/demo-upstream
go run ./cmd/prisma -config examples/08-graphql/prisma.yaml
```

```sh
gql() { curl -s -H "X-API-Key: ${KEY:-web-key}" -H 'Content-Type: application/json' -d "$1" localhost:8080/graphql | jq .; }
```

## Normal queries pass

```sh
gql '{"query":"{ me { name email } }"}'
```

## Field-level authorization

`User.ssn` needs `pii:read`. For `web-key` the field is stripped from the
query, and the rest still runs:

```sh
gql '{"query":"{ me { name ssn } }"}' | jq -c .
# {"data":{"me":{"name":"Ana"}},"extensions":{"receivedQuery":"query {\n\tme {\n\t\tname\n\t}\n}\n"}}
KEY=admin-key gql '{"query":"{ me { name ssn } }"}' | jq -c .data
# {"me":{"name":"Ana","ssn":"123-45-6789"}}
```

When a field is stripped, the gateway re-prints the query without it, which
is why `receivedQuery` is reformatted.

Mutations need `users:admin`, and are rejected outright without it:

```sh
gql '{"query":"mutation { deleteUser(id: \"1\") }"}'
# {"errors":[{"extensions":{"code":"FORBIDDEN"},"message":"access to field Mutation.* denied"}]}
```

The rules follow aliases, fragments and interfaces, so `{ x: me { s: ssn } }`
or a fragment on `User` can't get around them.

## Schema validation

Queries for fields that don't exist never reach the server:

```sh
gql '{"query":"{ me { password } }"}'
# {"errors":[{"extensions":{"code":"GRAPHQL_VALIDATION_FAILED"},"message":"Cannot query field \"password\" on type \"User\"."}]}
```

## Limits

```sh
# Too deep (maxDepth: 5)
gql '{"query":"{ me { friends { friends { friends { friends { friends { name } } } } } } }"}'
# query depth 7 exceeds limit 5 (QUERY_TOO_COMPLEX)

# Too expensive (maxCost: 200): list fields multiply the cost of their children
gql '{"query":"{ users(first: 100) { friends(first: 100) { name } } }"}'
# query cost 10101 exceeds limit 200

# Introspection is blocked
gql '{"query":"{ __schema { types { name } } }"}'
# introspection is disabled (FORBIDDEN)
```

Errors come back in the GraphQL envelope (`{"errors": [...]}`), which GraphQL
clients understand, rather than as an HTTP error page.

## Cost-based rate limiting

`cost: graphql` charges each request its computed cost, so a consumer gets
1000 points a minute whether it spends them on many cheap queries or a few
expensive ones. Watch the budget shrink:

```sh
curl -si -H 'X-API-Key: web-key' -H 'Content-Type: application/json' \
  -d '{"query":"{ users(first: 50) { name } }"}' localhost:8080/graphql | grep -i ratelimit-remaining
```

Batched requests (a JSON array of operations) are limited by `maxBatch`, and
each operation is checked on its own.
