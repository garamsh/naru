package command

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// Run runs naructl with args, the command line without the program's name.
func Run(args []string, stdout, stderr io.Writer) error {
	root := newRoot()
	root.SetArgs(append([]string{}, args...))
	root.SetOut(stdout)
	root.SetErr(stderr)
	return root.Execute()
}

const signerLong = `ca prints the certificates of the signer's authorities as PEM, each after a comment line with the
SHA-256 of its DER, which is what a seed's --orch-ca-sha256 takes for the orch authority. It needs
no person file: the certificates are public.

authorise-replica signs root's authorisation for the signer of a host to receive the authorities: pass the
file to naru-signer join. bundle writes the authorities sealed to the recovery key. restore fills a blank
signer from that bundle; the recovery key and its passphrase file are paths on the signer's host.
Those three are root's. reissue-root is the recovery key's, for an operator who lost root's person file:
it signs on this machine a once-only request for a new root certificate for the request --csr names,
and the signer revokes the old root. Its --recovery-key and --passphrase-file are local files.

rotate-host makes a new key for --host-id, has the signer issue a host certificate for it, installs the pair on
the host through its API (--host-client, a provisioner's client file) and, once the host reports to orch with it,
has the signer revoke every older serial it holds for the host. A failure before that leaves the old pair valid
and in use; the new key is sent to the host and written nowhere else. register-host records the serial of the certificate --cert, one gen node wrote offline, for a host
the signer did not admit, so that rotate-host can revoke it. lookup-host lists every serial the signer holds for --host-id and whether each is
revoked, and revoke-host revokes all of them in one signed list, for a host that is lost; a host the signer holds no serial for is refused, and
register-host is what records one. All of them are root's or an operator's.

Each takes exactly one of --socket, the one naru-signer serves on this host, and --config, an admin's client
file for a Node's API, which forwards to the signer of --endpoint. The signer checks every request's signature
itself, so the client file alone does not authorise anything.`

const operatorLong = `The admin file is root's (to add) or an operator's (to revoke) encrypted person file. A command that calls the
signer takes exactly one of --socket, the one naru-signer serves on this host, and --config, an admin's client
file for a Node's API, which forwards to the signer of --endpoint. The CSR is a DER or PEM PKCS #10 request the person made.`

// newRoot is the tree of naructl's commands. Every command is built afresh, so the values its flags
// hold are those of one run.
func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "naructl",
		Short:         "the operator's tool for a naru cluster",
		Long:          "naructl makes a cluster's authorities, each Node's user data and each operator's identity, and calls the Node API, the host API and orch.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
	}
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return fmt.Errorf("%w (see: %s -h)", err, c.CommandPath())
	})
	root.RunE = func(c *cobra.Command, args []string) error {
		if len(args) > 0 {
			return fmt.Errorf("unknown command %q", args[0])
		}
		_ = c.Help()
		return fmt.Errorf("a command is required")
	}
	root.AddCommand(
		genCommands(),
		leaf("version", "print the Node's version, boot ID and uptime", version),
		leaf("services", "list the Node's services and their state", services),
		leaf("logs <service>", "print a service's kept log, and with -f each new line", logs),
		parent("get", "read the Node's configuration", leaf("config", "print the Node's configuration and its status as YAML", getConfig)),
		leaf("apply", "merge a patch into the Node's configuration", apply),
		leaf("reboot", "stop the Node's services and reboot it", reboot),
		leaf("upgrade", "send a host upgrade bundle, or print the boot slot and how the last upgrade ended", upgradeCmd),
		leaf("reset", "remove the host's stored configuration or data by scope and reboot it", hostReset),
		hostCommands(),
		parent("microvm", "create, read and stop the microVMs a host runs",
			leaf("create", "create a microVM, from an image or in the OS form", microvmCreate),
			leaf("get <name>", "print a microVM", microvmGet),
			leaf("list", "list the microVMs", microvmList),
			leaf("stop <name>", "stop a microVM", microvmLifecycle("stop")),
			leaf("start <name>", "start a stopped microVM", microvmLifecycle("start")),
			leaf("delete <name>", "delete a microVM", microvmDelete),
			leaf("snapshot <name>", "snapshot a microVM", microvmSnapshot),
			parent("fence", "read and renew a microVM's fence",
				leaf("get <name>", "print a microVM's fence", fenceGet),
				leaf("renew <name>", "renew a microVM's fence", fenceRenew)),
		),
		parent("snapshot", "list, read, delete and resume the snapshots a host keeps",
			leaf("list", "list the snapshots", snapshotList),
			leaf("get <name>", "print a snapshot", snapshotGet),
			leaf("delete <name>", "delete a snapshot", snapshotDelete),
			leaf("resume <name>", "start a microVM from a snapshot", snapshotResume)),
		parent("orch", "set up, back up and restore orch",
			leaf("init", "write orch's recovery key, encrypted, and its public half", orchInit),
			leaf("ca", "print the orch authority's certificate and its SHA-256, from the signer", signerCA("orch")),
			leaf("backup", "take a sealed snapshot of orch now", orchBackup),
			leaf("restore", "open a sealed snapshot, and write or push the plain one", orchRestore),
			leaf("csr", "make orch's key and certificate request", orchCSR),
			leaf("issue", "issue orch's certificate through the signer", orchIssue)),
		parent("caller", "issue the credential of a use of the Machine API",
			leaf("credential", "issue a caller's certificate and key through the signer, as root", callerCredential)),
		parent("enroll-key", "create and revoke the keys that enroll hosts",
			leaf("create", "create an enrollment key", enrollKeyCreate),
			leaf("revoke <id>", "revoke an enrollment key", enrollKeyRevoke)),
		parent("group", "declare groups of microVMs orch keeps at their count",
			leaf("apply", "declare a group from a file", groupApply),
			leaf("get <name>", "print a group", groupGet),
			leaf("list", "list the groups", groupList),
			leaf("delete <name>", "delete a group", groupDelete)),
		parent("network", "declare the virtual networks orch gives every host",
			leaf("apply", "declare a network from a file", networkApply),
			leaf("get <name>", "print a network", networkGet),
			leaf("list", "list the networks", networkList),
			leaf("delete <name>", "delete a network", networkDelete)),
		parent("address-pool", "declare the LAN address ranges a network's exposures draw their addresses from",
			leaf("apply", "declare a pool from a file", addressPoolApply),
			leaf("list", "list the pools", addressPoolList),
			leaf("delete <name>", "delete a pool no exposure draws from", addressPoolDelete)),
		parent("exposure", "declare a LAN address that forwards to a network's members, list the addresses orch holds and release one",
			leaf("apply", "declare an exposure from a file", exposureApply),
			leaf("get <network> <id>", "print one exposure", exposureGet),
			leaf("list", "list every network's exposures", exposureList),
			leaf("delete <network> <id>", "start the release of one exposure's address", exposureDelete)),
		described(parent("operator", "issue and revoke operators through naru-signer, and make an identity's key",
			leaf("add", "issue an operator's certificate", operatorAdd),
			leaf("revoke", "revoke an operator's certificate", operatorRevoke),
			leaf("request", "make an identity's key and certificate request", operatorRequest),
			leaf("person", "write an identity's encrypted person file", operatorPerson)), operatorLong, ""),
		described(parent("signer", "use naru-signer: its authorities, bundle, replicas and root",
			leaf("ca", "print the signer's authorities", signerCA("orch", "operator", "host")),
			leaf("authorise-replica", "sign root's authorisation for a signer replica", signerAuthoriseReplica),
			leaf("bundle", "write the authorities sealed to the recovery key", signerBundle),
			leaf("rotate-host", "replace a host's key pair through the signer: install the new pair on the host, then revoke the old serials", signerRotateHost),
			leaf("register-host", "record the serial of a host's offline certificate so rotate-host can revoke it", signerRegisterHost),
			leaf("lookup-host", "list every serial the signer holds for a host ID", signerLookupHost),
			leaf("revoke-host", "revoke every serial the signer holds for a host ID, for a host that is lost", signerRevokeHost),
			leaf("reissue-root", "sign a request for a new root certificate with the recovery key", signerReissueRoot),
			leaf("restore", "fill a blank signer from a bundle", signerRestore)), signerLong, ""),
		parent("extension", "store, list and remove system extension images on a Node",
			leaf("add <naru-orch-ext-tag.sqfs>", "store a system extension image on the Node (the admin role)", extensionAdd),
			leaf("list", "list the images the Node stores and whether each is mounted", extensionList),
			leaf("remove <sha256>", "remove an image that is not mounted", extensionRemove)),
		leaf("events", "list orch's newest events, or one group's", events),
		leaf("console", "serve a web console on this workstation", consoleCmd),
	)
	return root
}

// hostCommands is host: the host API's information about the host itself, its networks, peers,
// tunnel and boot sets, and what orch does with the hosts of a fleet.
func hostCommands() *cobra.Command {
	return runnableParent("host", "print the host the host API answers for, and manage the hosts of a fleet", hostInfo,
		parent("boot-set", "add a release's boot set to the host, or remove one",
			leaf("add <naru-node-version.json>", "add the kernel and initramfs a release's manifest names, with no reboot", bootSetAdd),
			leaf("remove <name>", "remove a boot set", bootSetRemove)),
		parent("network", "the host's own networks (naructl network is the management plane's)",
			leaf("put <name>", "create or replace a network", hostNetworkPut),
			leaf("get <name>", "print a network", hostNetworkGet),
			leaf("list", "list the networks", hostNetworkList),
			leaf("delete <name>", "delete a network", hostNetworkDelete)),
		parent("peers", "the hosts this host's tunnel reaches",
			leaf("get", "print the peers", hostPeersGet),
			leaf("set", "replace the peers with a file's", hostPeersSet)),
		leaf("tunnel", "print the host's WireGuard tunnel", hostTunnel),
		leaf("list", "list the admitted hosts with the prefix orch allocated for each", hostList),
		leaf("pending", "list the hosts waiting to be admitted", hostPending),
		leaf("admit <id>", "admit a pending host", hostAdmit),
		leaf("label <id>", "replace a host's region and labels", hostLabel),
		leaf("drain <id>", "stop placing on a host and move its members off it", hostDrain("drain")),
		leaf("undrain <id>", "place on a host again", hostDrain("undrain")),
		leaf("install-seed", "write the NoCloud drive a new host boots the installer with (offline)", hostInstallSeed),
		parent("profile", "the fleet-wide inputs orch builds each joined host from",
			leaf("set", "give orch the host profile", hostProfileSet),
			leaf("get", "print orch's host profile", hostProfileGet)),
		described(leaf("remove <id>", "forget a host and free its Machine prefix", hostRemove),
			"remove forgets a host and frees its Machine prefix. Its certificate stays valid: operator revoke ends it.", ""),
	)
}

// genCommands is gen: what writes files, offline.
func genCommands() *cobra.Command {
	return parent("gen", "write authorities, client files and Node user data",
		leaf("api-ca", "write the Node API authority, api/ca.pem and api/ca.key", genAuthority(apiAuthority)),
		leaf("host-ca", "write the host API authority, host/ca.pem and host/ca.key", genAuthority(hostAuthority)),
		leaf("api-client", "write a client file for the Node API", genAPIClient),
		leaf("host-client", "write a client file for the host API (naru-hostd, port 7443)", genHostClient),
		leaf("host-profile", "write the fleet-wide inputs orch builds each joined host's configuration from", genHostProfile),
		leaf("node", "write a Node's user data", genNode),
	)
}
