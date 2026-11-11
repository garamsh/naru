# naru

naru is the node OS of the garamsh microVM platform. It installs on a machine, runs as that machine's init, and builds and manages the kernels the node runs. Its scope is set by the project owner's decisions in garamsh/discussions Discussion #3 (decisions 3 to 7).

## Scope

- **OS image**: the image a node boots.
- **init**: naru's own init as PID 1, written in Go. It does not use an existing distribution's init and holds no orchestration logic.
- **Node API**: a small API a node serves directly: apply config, upgrade, reset, status.
- **Installer**: the tool that installs naru on a machine.
- **Kernels**: naru builds and manages the host kernel and the guest kernel.

naru starts and watches the services its config names (mvmd, the gaon Node Agent and the gaon control plane), which run as host services and not inside microVMs. It has no control plane, API server or store of its own.

## Neighbouring projects

- **gaon** is the orchestration project. It calls naru's node API, and naru does not know gaon's resource API. Machine and cluster management are gaon's resources and controllers.
- **mvmd** is the node-local microVM runtime that naru starts as a host service.
- Dependencies are one-way: gaon to naru.
- Containers are not an execution unit anywhere in the project, development and CI included. The OS image is not a container image.

## Status

The repository is bootstrapped and holds no code. Image layout, upgrade scheme, node API shape, config format and boot and install paths are not decided. The shape of the repository is in `docs/architecture/structure.md`.

## Structure

- `AGENTS.md`: the contribution contract, who may change what and the rules that apply to every contributor
- `docs/convention/`: the conventions for code, reviews and documentation; `stack-go.md` is the stack file
- `docs/architecture/`: `structure.md` (how the repository is partitioned), the ADRs, and the index of both
- `docs/procedure/`: this repository's procedures for its recurring acts
- `.github/`: PR and issue templates, and CODEOWNERS
