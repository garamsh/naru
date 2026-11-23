# First hosts

The operator's steps from nothing to a control plane and the first hosts of a fleet, each host installed from one seed and admitted with its pairing code.

`TestFleet_TwoHostsInstalledFromOneSeedEnrollAreAdmittedWithTheirOwnIdentityAndRunAMicroVM` (`tooling/qemu/tests/join_fleet_test.go`) runs steps 6 to 9 under QEMU with two hosts, and the fleet tests of `tooling/qemu/tests` run steps 1 to 5 for the control hosts; the commands below are the ones those tests run. Where the test and a real fleet differ, the step says so. Every command prints a line when it succeeds; the line each step shows is the one to look for before the next step.

Values in angle brackets are yours. `<dir>` is a directory on the workstation that is kept private: it holds the authority keys until step 5 and the person files after it.

## The control plane

### 1. The authorities

The Node API authority signs the client files an operator reaches a control host's API with, and the host API authority signs the client files for a host's own API.

```
naructl gen api-ca --ca-dir <dir>
naructl gen host-ca --ca-dir <dir>
```

Each prints `naructl: wrote <dir>/api/ca.pem and <dir>/api/ca.key; keep ca.key in your secret store`, and the same with `host`. An admin client file for the first control host comes from the API authority:

```
naructl gen api-client --ca-dir <dir> --role admin --endpoint <first control host address> --out <dir>/admin.yaml
```

It prints `naructl: wrote <dir>/admin.yaml for role admin`. This file is the `--config` of the signer commands below. `gen node` writes `spec.api` into a host's user-data when `<dir>/api` holds the authority, so run these before step 3.

### 2. Orch's recovery key

```
naructl orch init --out-dir <dir> --passphrase-file <passphrase file>
```

It prints `naructl: wrote <dir>/recovery.key (encrypted; keep it and its passphrase apart from the control host) and <dir>/recovery.pub (give it to naru-orchd as --recovery-public-key)`.

### 3. The first control host's user-data

The first root identity is a key and a certificate request that `naru-signer init` signs the first time the signer starts:

```
naructl operator request --name root --der --key-out <dir>/root.key --csr-out <dir>/root.csr
```

It prints `naructl: wrote the key <dir>/root.key (0600) and the certificate request <dir>/root.csr for root`. Then the user-data:

```
naructl gen node --ca-dir <dir> --name <name> --address <cidr> --mac <mac> --gateway <ip> --nameserver <ip> \
  --api-server <ip> --machine-prefix <cidr> --guest <naru-guest-version.json> --firecracker-sha256 <hex> \
  --control-plane --extension naru-orch=<sha256 of naru-orch-ext-TAG.json> \
  --signer --signer-recovery-public <dir>/recovery.pub --signer-root-csr <dir>/root.csr \
  --signer-peer-listen <address>:<port> --signer-peer-name <address> \
  --out <dir>/<name>/naru.yaml --network-config <dir>/<name>/network-config
```

It prints `naructl: wrote <dir>/<name>/naru.yaml` and the network config's path. Boot the host from the naru-host image with that user-data as its NoCloud drive. The signer's first start logs the first root certificate and the revocation public key as PEM blocks on the console; copy both from there (`docs/architecture/signer.md`).

### 4. The signer's root and orch's certificate

Join the root certificate to its key as the encrypted person file, then have orch's key and certificate request made and root sign the certificate through the Node API (`--config`), which forwards to the signer's socket and adds no authority:

```
naructl operator person --key <dir>/root.key --cert <dir>/root.crt --passphrase-file <passphrase file> --out <dir>/root.person
naructl orch csr --name orch-1 --dns orch-1 --ip <address> --key-out <dir>/orch-1.key --csr-out <dir>/orch-1.csr
naructl orch issue --admin <dir>/root.person --passphrase-file <passphrase file> --csr <dir>/orch-1.csr --name orch-1 \
  --config <dir>/admin.yaml --out <dir>/orch-1.crt
```

The lines to look for: `naructl: wrote <dir>/root.person for root (root, serial <serial>) and removed the plain key`, then `naructl: issued an orch certificate for orch-1, serial <serial>, valid until <time>; wrote <dir>/orch-1.crt`.

Orch's `spec.orch` needs the authority orch's certificate chains to, and the seed in step 7 needs its SHA-256. `naructl signer ca` and `naructl orch ca` print them, taken from the signer through the Node API's admin role (`--config` is the admin client file, and `--endpoint` is the first control host's Node API address when the file names more than one):

```
naructl signer ca --config <dir>/api/admin.yaml
naructl orch ca --config <dir>/api/admin.yaml
```

`signer ca` prints the `orch`, `operator` and `host` authorities, and `orch ca` the orch authority alone, each as a line `# <name> authority, SHA-256 <hex of its DER certificate>` and the PEM. Save the orch authority's PEM as the orch authority file below, and put the operator authority's PEM and the host authority's PEM in the same file. `spec.orch.ca` is the only trust orch checks client certificates against, so it must hold the operator authority for step 10 and the signer's host authority, which issues the certificate a joined host sends with its heartbeat: without it orch answers every heartbeat with 401 `a client certificate is required` and the host's LAST SEEN stays `never` ([#322](https://github.com/garamsh/naru/issues/322)). Keep the host authority's PEM for one more use: orch verifies the hosts' API certificates against `spec.orch.hostCA`, which must hold it (the signer issues a joined host's certificate from that authority) beside the host API authority of step 1; the hash in that line is the one the seed carries. Orch serves no authority of its own, so `orch ca` reads it from the signer, as the signer's `GET /v1/ca` gives it.

Edit the first host's user-data to add orch (`spec.orch` and the file that carries its key, whose shapes are `lib/init/config`'s) and boot it again. Orch answers once its API does: `naructl host pending --orch-url https://<address>:7500 --orch-ca <orch authority file> --identity <dir>/root.person --passphrase-file <passphrase file>` prints the heading `HOST CODE ADDRESS FIRST SEEN HARDWARE` and no row. Every `naructl` command that talks to orch below takes those four flags, `--orch-url`, `--orch-ca`, `--identity` and `--passphrase-file`, which are left out for brevity.

### 5. The other two control hosts

Each is a `gen node` with `--control-plane --signer --signer-replica` in place of `--signer-recovery-public` and `--signer-root-csr`, and `--member <name>=<address>` for each of the three, with the virtual address as `--api-server`. A replica signer logs its join key on the console; root authorises it, and the host's user-data takes the authorisation as `spec.signer.replica.join`:

```
naructl signer authorise-replica --admin <dir>/root.person --passphrase-file <passphrase file> \
  --host-id naru-<name> --join-key <file holding the join key> --out <dir>/authorisation-<name>
```

It prints `naructl: wrote root's authorisation for the signer of naru-<name> to <dir>/authorisation-<name>; it expires in <duration>`. The authority keys are not on the workstation after step 5: remove `<dir>/api/ca.key` and the other `ca.key` files to the secret store.

## The fleet's inputs

### 6. The host profile

The profile holds what every host admitted from now on is built from: the guest manifest, the Firecracker hash, the Machine pool, orch's address and authority hash, the registry the hosts pull images from, and the management network.

```
naructl gen host-profile --guest <naru-guest-version.json> --firecracker-sha256 <hex> --machine-pool <cidr> \
  --orch-url https://<address>:7500 --orch-ca-sha256 <hex> --registry host=<host:port>,ca=<PEM file> \
  --host-management-prefix <cidr> --host-egress-interface <interface> --out <dir>/host-profile.json
naructl host profile set -f <dir>/host-profile.json
```

The lines to look for: `naructl: wrote <dir>/host-profile.json (mode 0644); load it with host profile set -f` (mode 0600 when the registry has a password), then `naructl: orch keeps the host profile; hosts admitted from now on are built from it`.

### 7. The seed

One enrollment key and one seed serve every host of a batch:

```
naructl enroll-key create --ttl 24h
naructl host install-seed --orch-url https://<address>:7500 --orch-ca-sha256 <hex> --enroll-key <file holding the secret> --out <dir>/seed.iso
```

`enroll-key create` prints `id`, `expires` and `secret` lines, and `The secret is shown once; orch keeps only its hash.`; save the secret to a file with mode 0600. `install-seed` is offline and prints `naructl: wrote <dir>/seed.iso; attach it to the host as a CD or a disk and boot the installer from the naru-host image`. The seed carries the key, so keep it as private as the key. `naructl enroll-key revoke <id>` prints `naructl: revoked enrollment key <id>` and ends the key: a host that boots from the seed afterwards is refused (`orch refused the enrollment` on its console) and does not appear in `host pending`.

## Joining and admitting a host

### 8. Boot the host

On Proxmox, for each host:

1. Import the release's `naru-host-<version>.qcow2` as the host's boot disk.
2. Attach `seed.iso` as a CD-ROM drive.
3. Add a second, blank disk: it becomes the host's data disk. The host does not need a disk with files on it.
4. Give the host a DHCP reservation for its network interface's MAC address, the management address you want it to have, with a router, a DNS server and an NTP server. The host takes its address, router, DNS and time source from DHCP, so a real fleet needs a reservation per host. The fleet test serves one from the test process (`internal/dhcpserver`) that answers only the MACs it holds a reservation for, in the place of your DHCP server.
5. Start it.

The host enrolls and prints, on its console, `naru-hostd: host h-<16 hex digits> waits for admission, pairing code <code>`. The same seed boots every host of the batch; each makes its own key, so each shows its own ID and code.

`naructl host pending` lists them:

```
HOST CODE ADDRESS FIRST SEEN HARDWARE
h-<id> <code> <address> <time> 4 CPU, 3072 MiB, 1 disk(s), 1 link(s)
```

The hardware column is what the host reported: use it, with the address and the ID on the console, to tell which machine is which before admitting.

### 9. Admit the host

Match the code on the host's console to the row, then:

```
naructl host admit h-<id> --code <code from the console> --region <region> --label <key>=<value>
```

It prints `naructl: admitted host h-<id> in region <region>`. Orch has the signer issue the host's certificate, builds the host's configuration from the host profile and delivers it to the host, which applies it and reboots: its console shows `join configuration applied revision=1`, and `naructl host pending` no longer lists it. The host then reports to orch: `naructl host list` shows its region, labels, the Machine prefix orch allocated to it and a non-zero `LAST SEEN` (`GET /v1/hosts` carries the same, with `machinePrefix`), and a group whose `requiredLabels` match places a microVM on it.

Admit each host of the batch the same way, and revoke the enrollment key once the batch has joined.

### 10. Read the host with an operator certificate

A joined host accepts the certificate `naructl operator add` issued, with the role of its unit: `root` and `operator` provision, `reader` and `automation` read. Save the host authority's PEM (`naructl signer ca --authority host`), and run `naructl host --identity <dir>/<name>.person --passphrase-file <passphrase file> --host-ca <host authority file> --endpoint <address> --server-name <h-id>`. The same flags go on `naructl microvm`, `naructl snapshot` and the other host commands; a reader's write is refused with 403, and a client of another authority is refused by the handshake.

### The seed may be detached

Once a host is admitted and shows `join configuration applied`, it no longer needs the seed. From v0.1.0-dev.12 init, with no seed attached, finds the data disk by its default label `NARUDATA`, runs the configuration orch delivered and brings the network up as the last boot with the seed did. Detach the seed CD-ROM and restart the host to do it, and destroy the seed file and the key's secret. The enrollment key may be revoked while the seed is attached or after.

- A host on v0.1.0-dev.11 cannot boot without its seed. Its key may be revoked while the seed stays attached; only from dev.12 may the seed also be detached.
- A host that was admitted on dev.11 must boot once on dev.12 with its seed attached before the seed is detached: init stores the seed's `network-config` at that boot. Detached before, it refuses with `no stored network-config; attach the seed for one boot`.
- With no seed and no stored configuration, or with a data disk labelled other than `NARUDATA`, init refuses and says so on the console. Attach the seed again, which wins as before.
