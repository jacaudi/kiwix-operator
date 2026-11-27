# Migration

This is a task guide to moving an existing Kiwix library, a PVC full of ZIM
files, under the operator without re-downloading anything.

## Adopt the Existing Volume

1. Create a `KiwixLibrary` that names the existing PVC:

   ```yaml
   apiVersion: kiwix.jacaudi.dev/v1alpha1
   kind: KiwixLibrary
   metadata:
     name: library
   spec:
     storage:
       existingClaim: kiwix
   ```

2. Wait for `Available`. On first start the librarian scans `/data` and serves
   every top-level `*.zim` as is. A file that `kiwix-manage add` rejects, for
   example a truncated one, is listed in `status.unmanagedFiles` with its
   error and not served.

3. For each file you want managed, create a `ZimArchive` with `adoptFile` set
   to that file's name:

   ```yaml
   apiVersion: kiwix.jacaudi.dev/v1alpha1
   kind: ZimArchive
   metadata:
     name: wikipedia-en-100
   spec:
     libraryRef:
       name: library
     adoptFile: wikipedia_en_100_2025-10.zim
     source:
       download:
         catalog:
           name: wikipedia_en_100
           flavour: full
   ```

   The file is renamed to `/data/wikipedia-en-100.zim`, which is a one-time
   URL change. The archive then serves it as its current version and later
   refreshes are gated against it.

4. **Remove `adoptFile` once the archive is served.** After the archive has a
   current version the field is ignored, but left in Git it would stall a
   disaster recovery on a fresh volume: while `adoptFile` is set and there is
   no current version, the archive never fetches.

## Rules to Know

- **Names.** `/data/<archive>.zim`, where `<archive>` is a DNS label of at
  most 45 characters, belongs to the `ZimArchive` of that name. Every other
  top-level `*.zim` is unmanaged and served as is.
- **`adoptFile` is a file name**, matching `^[A-Za-z0-9][A-Za-z0-9._-]*\.zim$`,
  of an existing unmanaged regular file. Symlinks are rejected.
- **A missing file** sets `Degraded/AdoptFileMissing` and
  `Available=False/AwaitingAdoption`, and notifies once. An invalid name sets
  `AdoptFileInvalid`. In both cases the archive waits and does not fetch; clear
  the field to fetch instead.
- **Two archives naming one file:** the older archive wins and the other gets
  `Degraded/AdoptConflict`.
- **Duplicates.** A fetch whose ZIM has the same UUID as an unmanaged file is
  rejected as `DuplicateBook`. The message names the file; adopt it.
- **Re-creating an archive** after a `Retain` deletion inherits the retained
  file, and its next fetch is a refresh gated against it.
- **A claim can have one library.** A second `KiwixLibrary` naming a claim
  another library uses gets `Degraded/ClaimInUse`; the older library wins.

[← Back to README](../README.md)
