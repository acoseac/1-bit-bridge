# Config lifecycle history

## B514 — durable-writer evidence and advice (2026-10-23)

Three repeated A,B,A observations cannot establish that an overlay's upper
path reaches another filesystem: XFS inode totals and ZFS block totals can
change and return between reads. The writer retains three bounded retries;
upper {B,B,A} against overlay {A,A,A} is accepted on the third upper read.
A mismatch after all three stays ErrMediumUnknown and says inconclusive,
with advice to retry after filesystem activity settles and check the
service's mount namespace. This deliberately does not certify an XFS/ZFS
path whose relationship to the overlay cannot be established.

Linux reads st_dev with stat and renders its major:minor. The upper path's
own mount ID authoritatively selects its mountinfo line and that line's
fstype is named as evidence. A decoy mount-point spelling is ignored.
Device numbers are detail only, not a second identity gate: a btrfs
subvolume's st_dev can be its anonymous device while the line names the
superblock's device. A difference, or unavailable st_dev, does not make a
valid mount-ID-selected line inconclusive. Failed or absent mount ID and
missing or malformed lines leave the visible filesystem unknown; the read
error is wrapped. Round 2 corrects round 1's device-equality requirement.
This evidence identifies the filesystem visible here; it cannot authenticate
an upper backing hidden in another mount namespace. Existing agreement and
moving-totals checks, volatile refusals and nested-overlay depth bound stay.
The current inner mount's ID also selects nested overlay and FUSE lines.

ErrOverlayBind still matches ErrMediumUnknown, but describes unreachable
copy-up parents on a writable overlay subtree bind: persistent storage alone
does not mend its crash risk. No upper on the root's own mount (depth 0)
means no copy-up, so a read-only bind passes and leaves EROFS to the write.
At depth > 0, a read-only overlay is ErrMediumUnknown: the visible upper
path cannot be a writable overlay's upper layer. This includes a writable
bind whose upper path has been covered by a read-only overlay, or mapped
there in the service's mount namespace. Round 1's unconditional nil bypassed
the bind refusal and also accepted this impossible upper on an ordinary
writable overlay. Both shapes are fixture regressions in
TestMediumRefusesWhatDoesNotOutliveAReboot. syncAbove names every
failed directory, advises read access or leaving the overlay only for
fs.ErrPermission, and wraps other open errors without that advice. Its doc
names copyUpsFrom, which computes the directories to sync.

The platform-neutral tests exercise the mount-table evidence and totals
sequences without host proc/sys reads. TestMediumMismatchesNeedStableEvidence
includes btrfs with differing device numbers: it names btrfs and both
numbers, never claims a device mismatch defeats the mount ID, and still
answers ErrMediumUnknown for A,B,A. Linux's adapter test uses only a temp
directory and compares its stat device with linuxStatfs's output. The engine
state machine, wire, and macOS/Windows mount readers are unchanged.
