# lib/atomicfile

Replaces a file so that a reader, and a crash, sees the old file or the new one and never a part of it.

- `atomicfile.Write(path, data, perm, opts...)` writes a temporary file in the directory of `path`, syncs it, renames it over `path` and syncs the directory. `atomicfile.Owner(uid, gid)` gives the file an owner first.
- `atomicfile.SyncDir(dir)` makes the entries of a directory durable, for a caller that renames or removes without `Write`.
- A crash can leave a temporary file named `atomicfile.TempPrefix` + a random suffix; a caller that owns the directory may sweep them.

Linked by `host/` and `lib/init`, and by `orch/` and `tooling/qemu` (`docs/architecture/structure.md`).
