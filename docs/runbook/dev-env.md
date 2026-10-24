# A local apiserver for development

`make dev` runs the apiserver on `127.0.0.1:8080` in dev mode, with its
database, admin socket and a key-encryption key of its own under
`.cache/dev/`, which git ignores. The first run makes the key; delete
`.cache/dev/` to start over with an empty store.

In dev mode the admin socket takes the user the apiserver runs as as root
on the host, so the first admin token is made from a second terminal with:

```sh
curl -sS --unix-socket .cache/dev/admin.sock -H 'Content-Type: application/json' \
  -d '{"name":"admin","role":"ROLE_ADMIN"}' \
  http://tower/tower.iam.v1alpha1.ServiceTokenIssuerService/CreateServiceToken
```

The answer holds the token's secret, `twr_` and 43 more characters, once:
nothing shows it again. Send it as `Authorization: Bearer <secret>` on the
public API. In dev mode the API also takes `X-Tower-Dev-Principal:
serviceToken:<name>` in place of the secret, as the token of that name, with
its role.

For example, the first project, as the admin token:

```sh
curl -sS -H 'Content-Type: application/json' -H 'X-Tower-Dev-Principal: serviceToken:admin' \
  -d '{"project":{"metadata":{"name":"web"}}}' \
  http://127.0.0.1:8080/tower.tenancy.v1alpha1.ProjectService/ApplyProject
```
