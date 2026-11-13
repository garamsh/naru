# ADR 0029: aws-credentials is a Node service that serves the IAM Roles Anywhere session to mvmd as the instance metadata endpoint, a service restarts when a file it reads is replaced, and init follows the ECR helper's log into mvmd's

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#84, part of garamsh/aurora#749. It supersedes item 3 (the `aws/config` file) and the `AWS_CONFIG_FILE` half of item 4 of [ADR 0026](./0026-a-node-takes-registry-credentials-and-trust-as-configuration-an-ecr-credential-comes-from-iam-roles-anywhere-and-an-agent-pair-without-the-required-organization-is-set-aside.md) by reference: its body is not edited, and the rest of it holds (the registries, the pair, the bundle, the set-aside rule, the redaction). Item 10's statement that `spec.aws` is live and restarts nothing is amended below.

## Context

- **A Node at v0.1.0-dev.6 cannot pull from ECR.** ADR 0026 had the AWS SDK run `aws_signing_helper credential-process` from a `credential_process` line. The SDK runs that line through `sh -c`, and the image has no shell, so the SDK fails before the helper starts.
- **The helper's own log is unreadable.** `docker-credential-ecr-login` writes its log to `$HOME/.ecr/log/ecr-login.log`, and mvmd, started by init with an exact environment, has no `HOME`.
- **`aws_signing_helper serve`** (v1.8.6) serves the instance metadata endpoint on `127.0.0.1` only (default port 9911, `--port`), and refuses a request whose `Host` is not `127.0.0.1:<port>` or `localhost:<port>`. It reads the certificate and key once, at start, makes its first session at once, and exits 1 with `failed to generate initial credentials: ...` when that fails. The AWS SDK reads its credentials from an endpoint named by `AWS_EC2_METADATA_SERVICE_ENDPOINT` without a shell.
- **init had no way to restart a service because a file it reads changed.** `restart` on `spec.files` and `spec.pki` covers files init writes; the Node Agent writes `node-agent.pem` when it renews, and ADR 0026's `keptPair` replaces it by a rename.
- **The ECR helper's log is a file, not a stream**, so the Node API, which reads a service's console lines, does not carry it.

## Decision

1. **`aws-credentials` is a service of the layout, present only when `spec.aws` is set.** `naructl` adds it after `gaon-node-agent`, so that init checks the agent's kept pair before the helper reads it. Its binary is `/bin/aws_signing_helper` and its arguments are `serve --certificate <pair> --private-key <pair> --trust-anchor-arn ... --profile-arn ... --role-arn ... --region ... --port 9911`, `<pair>` being `spec.aws`'s `certificate` and `key` as `naructl` defaults them (the Node Agent's `node-agent.pem`). It has no environment. A Node without `spec.aws` runs no such service.
2. **The port is 9911**, the helper's default and a constant of the layout (`AWSCredentialsPort`). No other service of a Node uses it: etcd's are 2379 and 2380, the registry stand-in's is 5000, the Node API's is its own, and gaon's are not 9911. The helper binds `127.0.0.1` only (v1.8.6's `serve` has no bind option; read from its source at the pinned commit); no test shows it, because the helper exits before it listens when its first session fails, and no test reaches a real session.
3. **mvmd's environment is `PATH`, `HOME`, and with `spec.aws`, `AWS_EC2_METADATA_SERVICE_ENDPOINT=http://127.0.0.1:9911` and `AWS_REGION`.** There is no `AWS_CONFIG_FILE` and init writes no `aws/config`; the `aws` member of the registry files is gone, and the apply engine no longer compares it. The endpoint-mode variable is not set: the SDK takes the endpoint variable alone.
4. **`HOME` is `/run/naru/registry/home`, which init makes with mode 0700 with the other registry files** (and sets to 0700 whatever mode it had). The helpers mvmd runs write there.
5. **A service may name `restartOnChange`, a list of absolute, clean paths, and init restarts the service when the content of one changes.** The manager takes the SHA-256 of each file (a missing file is its own digest) when it starts the service and every 5 seconds while it runs; a difference stops the service and starts it again at once, with no backoff, logged `a watched file changed, restarting`. It is an addition to the service's declaration, compared by `Equal`, and a configuration without it parses as before. `aws-credentials` names `<pair>`, so a renewal by the agent and a replacement of an O-less pair both restart it, and it reads the new pair. This is the mechanism for any service that reads a file something other than init writes.
6. **A service may name `logFiles`, a list of absolute, clean paths, and init follows them into the service's console lines.** The launcher notes each file's size when it starts the service, reads what is appended every 500 ms, writes complete lines only and keeps a partial one, restarts from the start when the file shrinks, and reads once more when the service's output closes. mvmd's `logFiles` is `$HOME/.ecr/log/ecr-login.log`. **The operator reads the helper's log with `naructl logs mvmd`**, an existing read of the Node API's roles; no new route and no new role. The lines carry no tag of their own.
7. **`spec.aws` is still in the live class, and a change restarts what it changes.** The service's arguments and mvmd's environment are in the layout, so a configuration whose `spec.aws` changed and whose services did not is changed by the operator's patch, as ADR 0018 has it for any service. Apply restarts `aws-credentials` (its arguments differ) and mvmd (its environment differs). `spec.aws` alone no longer plans an action, because it no longer names a file init writes; `naructl gen node` writes all three together.
8. **No shell is added to the image.** The one new thing in the image's behaviour is `aws_signing_helper serve`, already in the image.

## Consequences

- A Node with `spec.aws` pulls from ECR with a credential the SDK reads from a loopback endpoint; no process of the Node runs through `sh`.
- A failure of the helper is the helper's own line in `naructl logs aws-credentials`; a failure of the ECR helper is its line in `naructl logs mvmd`.
- The loopback port is a credential source for every process of the Node, not mvmd's alone: any process that can reach `127.0.0.1:9911` with a `Host` of the loopback gets a session. The Node's processes are its own services and the MicroVMs' supervisors; a MicroVM's guests are in another network. The session lasts 900 s and the role is the aurora role's.
- With the helper exiting while AWS is unreachable, init restarts it with its backoff (1 s doubling to 30 s) until a session succeeds.
- A watched file is polled every 5 seconds: a replaced pair is read within that interval plus the restart.
- A Node whose user-data is of v0.1.0-dev.6 form (no `aws-credentials`, an `AWS_CONFIG_FILE` in mvmd's environment) still boots; it has no working ECR credential until its configuration is rewritten.

## Signs the decision was wrong

- A second service needs a credential on a loopback port and the port constants collide or multiply: the endpoint belongs in a general credential service of init.
- The set of files to watch grows past a handful, or a 5-second poll is too slow: init needs a change notification (inotify) instead of a digest.
- A process that must not hold the Node's session can reach the port: the endpoint needs a token (IMDSv2) and mvmd the means to send it, or the helper a socket of its own.
- The operator needs the ECR helper's log apart from mvmd's lines: a service gets a named stream, and `naructl logs` a second name.
