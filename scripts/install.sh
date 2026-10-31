#!/bin/sh
# The 1-bit Bridge installer for Linux: moOde, Raspberry Pi OS, Volumio, Debian,
# Ubuntu and the rest, on amd64 or arm64.
#
#   curl -fsSL https://1-bit.app/install.sh | sh
#   curl -fsSL https://1-bit.app/install.sh | sh -s -- --library /mnt/USB
#
# Run it as the user the bridge should run as, not as root. On a box with
# nothing of the bridge on it, it downloads the release it came with (with
# curl, held to HTTPS), checks the archive's SHA-256 against the release's
# checksums.txt, installs the program as ~/.local/bin/bridge, and runs
# `bridge init`, which writes the settings and installs the per-user systemd
# service; it asks for sudo once, and says why first, to turn on lingering so
# the service outlives the login and starts at boot. On a box where the bridge
# is installed as that service, it upgrades it through the bridge's own
# `bridge update`, which checks the paired devices first and keeps the
# previous program for a rollback. Anything else it refuses, with nothing
# changed, saying what to do instead. `--help` lists the options and exit
# codes.
#
# The script is function definitions and, last, one compound command. A
# download cut short anywhere before that command's closing brace is a syntax
# error at its end, and the shell runs nothing of it.
#
# BRIDGE_INSTALL_BASE_URL replaces the release download base (https only).
# The BRIDGE_INSTALL_TEST_* variables exist for scripts/install-test.

# constants sets what does not depend on the box.
constants() {
	# The release this script came with. scripts/stamp-install.sh writes it
	# here when a release is built; the repository's copy is unstamped and
	# installs only a release named with --version.
	STAMPED_VERSION=''
	REPO_URL='https://github.com/acoseac/1-bit-bridge'
	UNIT_NAME='com.acoseac.1-bit-bridge.service'
	DOWNLOAD_BASE=${BRIDGE_INSTALL_BASE_URL:-$REPO_URL/releases/download}
	SYSROOT=${BRIDGE_INSTALL_TEST_SYSROOT:-}
	TTY_DEVICE=${BRIDGE_INSTALL_TEST_TTY:-/dev/tty}
	case $DOWNLOAD_BASE in
	https://*) ;;
	*) leave 2 "BRIDGE_INSTALL_BASE_URL must be an https:// URL." ;;
	esac
}

say() {
	printf '%s\n' "$@"
}

warn() {
	printf '%s\n' "$@" >&2
}

# leave CODE LINE...: prints the lines on stderr and exits with CODE. Not
# named stop: ksh93 defines stop as an alias of kill -s STOP, and an alias
# wins over a function, so a call "stop 1" would send SIGSTOP to process 1.
leave() {
	leave_code=$1
	shift
	printf '%s\n' "$@" >&2
	exit "$leave_code"
}

usage() {
	cat <<EOF
Usage: sh install.sh [options]
       curl -fsSL https://1-bit.app/install.sh | sh -s -- [options]

Installs the 1-bit Bridge on this Linux box as a service of the user who runs
it, or upgrades the bridge installed that way through \`bridge update\`.
Run it as that user, not as root.

Options:
  --library PATH   the music folder to serve; without it, the installer asks
                   when there is a terminal, and refuses otherwise
  --version X.Y.Z  install that release; this copy installs $(stamp_text).
                   A new install only: an installed bridge moves to the latest
                   release through bridge update
  --no-init        install the program only, and set nothing up; a new
                   install only
  --uninstall      print the commands that remove the bridge; changes nothing
  -h, --help       this text

Exit codes:
  0  done
  1  a step failed or was stopped; what it changed, if anything, is said
  2  a usage error
  3  refused, with nothing changed: it says why, and what to do instead
  4  incomplete: the program is installed, but not set up, or its service
     was not seen serving the release; what to run next is printed
EOF
}

stamp_text() {
	if [ -n "$STAMPED_VERSION" ]; then
		printf 'release %s' "$STAMPED_VERSION"
	else
		printf 'only the release --version names'
	fi
}

usage_error() {
	warn "$1" "The options are in --help."
	exit 2
}

# normalize_version VALUE prints VALUE without a leading v when it is a release
# version (X.Y.Z, optionally -suffix), and fails otherwise.
normalize_version() {
	nv_value=${1#v}
	case $nv_value in
	'' | *[!0-9A-Za-z.-]*) return 1 ;;
	esac
	printf '%s\n' "$nv_value" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$' || return 1
	printf '%s' "$nv_value"
}

parse_args() {
	OPT_VERSION=''
	OPT_LIBRARY=''
	OPT_NO_INIT=0
	OPT_UNINSTALL=0
	pa_others=0
	while [ "$#" -gt 0 ]; do
		case $1 in
		--version)
			if [ "$#" -lt 2 ] || [ -z "$2" ]; then
				usage_error "--version needs a release, such as 0.2.2."
			fi
			OPT_VERSION=$2
			pa_others=1
			shift 2
			;;
		--version=*)
			OPT_VERSION=${1#--version=}
			[ -n "$OPT_VERSION" ] || usage_error "--version needs a release, such as 0.2.2."
			pa_others=1
			shift
			;;
		--library)
			if [ "$#" -lt 2 ] || [ -z "$2" ]; then
				usage_error "--library needs a folder."
			fi
			OPT_LIBRARY=$2
			pa_others=1
			shift 2
			;;
		--library=*)
			OPT_LIBRARY=${1#--library=}
			[ -n "$OPT_LIBRARY" ] || usage_error "--library needs a folder."
			pa_others=1
			shift
			;;
		--no-init)
			OPT_NO_INIT=1
			pa_others=1
			shift
			;;
		--uninstall)
			OPT_UNINSTALL=1
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		--)
			shift
			[ "$#" -eq 0 ] || usage_error "Unexpected argument: $1"
			;;
		*)
			usage_error "Unknown option: $1"
			;;
		esac
	done
	if [ "$OPT_UNINSTALL" = 1 ] && [ "$pa_others" = 1 ]; then
		usage_error "--uninstall takes no other option."
	fi
	if [ -n "$OPT_VERSION" ]; then
		pa_version=$(normalize_version "$OPT_VERSION") || usage_error "--version $OPT_VERSION is not a release version, such as 0.2.2."
		OPT_VERSION=$pa_version
	fi
	if [ -n "$OPT_LIBRARY" ]; then
		case $OPT_LIBRARY in
		/*) ;;
		*) OPT_LIBRARY=$PWD/$OPT_LIBRARY ;;
		esac
		[ -d "$OPT_LIBRARY" ] || usage_error "--library $OPT_LIBRARY is not a folder."
	fi
}

# setup_cleanup arranges that every exit, a signal's included, removes the
# temporary directory and a staged binary, and the directories this run made
# for a binary it did not install.
setup_cleanup() {
	TMP_DIR=''
	STAGE=''
	CREATED_LOCAL=''
	CREATED_BIN=''
	INSTALLED=0
	trap cleanup EXIT
	trap 'exit 129' HUP
	trap 'exit 130' INT
	trap 'exit 143' TERM
}

cleanup() {
	cleanup_status=$?
	trap - EXIT
	if [ -n "$STAGE" ]; then
		rm -f "$STAGE" || :
	fi
	if [ -n "$TMP_DIR" ]; then
		rm -rf "$TMP_DIR" || :
	fi
	if [ "$INSTALLED" = 0 ]; then
		if [ -n "$CREATED_BIN" ]; then
			rmdir "$CREATED_BIN" 2>/dev/null || :
		fi
		if [ -n "$CREATED_LOCAL" ]; then
			rmdir "$CREATED_LOCAL" 2>/dev/null || :
		fi
	fi
	exit "$cleanup_status"
}

# os_release KEY prints KEY's value from os-release, its quotes removed.
os_release() {
	or_file=$SYSROOT/etc/os-release
	[ -r "$or_file" ] || or_file=$SYSROOT/usr/lib/os-release
	[ -r "$or_file" ] || return 0
	sed -n "s/^$1=//p" "$or_file" | head -n 1 | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\\(.*\\)'\$/\\1/"
}

# userland_bits prints 32 or 64: the word size of this system's programs,
# which a 64-bit kernel does not tell (Raspberry Pi OS's 32-bit image boots a
# 64-bit kernel on a Pi 4 or 5).
userland_bits() {
	ub_bits=$(getconf LONG_BIT 2>/dev/null) || ub_bits=''
	case $ub_bits in
	32 | 64)
		printf '%s' "$ub_bits"
		return 0
		;;
	esac
	# The ELF class byte of /bin/sh: 1 is 32-bit, 2 is 64-bit.
	ub_class=$(od -An -tx1 -j4 -N1 "$SYSROOT/bin/sh" 2>/dev/null | tr -d ' ') || ub_class=''
	case $ub_class in
	01) printf '32' ;;
	*) printf '64' ;;
	esac
}

detect_distro() {
	dd_pretty=$(os_release PRETTY_NAME)
	dd_id=$(os_release ID)
	[ -n "$dd_pretty" ] || dd_pretty=Linux
	if [ -d "$SYSROOT/volumio" ] && [ -d "$SYSROOT/data/configuration" ]; then
		DISTRO=volumio
		dd_version=$(os_release VOLUMIO_VERSION)
		DISTRO_LINE="Volumio${dd_version:+ $dd_version}"
	elif [ -x "$SYSROOT/usr/local/bin/moodeutl" ]; then
		DISTRO=moode
		DISTRO_LINE="moOde audio player, on $dd_pretty"
	elif [ "$dd_id" = raspbian ] || [ -e "$SYSROOT/etc/rpi-issue" ]; then
		DISTRO=raspios
		DISTRO_LINE="Raspberry Pi OS ($dd_pretty)"
	elif [ "$dd_id" = ubuntu ]; then
		DISTRO=ubuntu
		DISTRO_LINE=$dd_pretty
	elif [ "$dd_id" = debian ]; then
		DISTRO=debian
		DISTRO_LINE=$dd_pretty
	else
		DISTRO=linux
		DISTRO_LINE=$dd_pretty
	fi
}

check_platform() {
	cp_os=$(uname -s)
	case $cp_os in
	Linux) ;;
	Darwin)
		leave 3 "This installer is for Linux. On a Mac, download the macOS archive (signed and notarized) from $REPO_URL/releases," \
			"unpack it with tar -xzf, and run bridge init from the folder it unpacks to."
		;;
	MINGW* | MSYS* | CYGWIN* | Windows_NT)
		leave 3 "This installer is for Linux. On Windows, download the Windows zip from $REPO_URL/releases," \
			"unpack it, and run bridge.exe init."
		;;
	*)
		leave 3 "This installer is for Linux, and this system is $cp_os." \
			"The releases at $REPO_URL/releases have macOS, Linux and Windows builds."
		;;
	esac
	cp_machine=$(uname -m)
	case $cp_machine in
	x86_64 | amd64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	arm*)
		leave 3 "This box runs a 32-bit ARM system ($cp_machine), and the bridge is built for 64-bit systems only (arm64 and amd64)." \
			"On a Raspberry Pi 3, 4 or 5, a 64-bit system runs it: Raspberry Pi OS (64-bit), or moOde's 64-bit image."
		;;
	i?86 | x86)
		leave 3 "This box runs a 32-bit x86 system ($cp_machine), and the bridge is built for 64-bit systems only (amd64 and arm64)."
		;;
	*)
		leave 3 "This box is $cp_machine, and the bridge is built for amd64 and arm64 only."
		;;
	esac
	detect_distro
	cp_note=''
	if [ "$ARCH" = arm64 ] && [ "$(userland_bits)" = 32 ]; then
		cp_note=" (a 64-bit kernel with a 32-bit system: the bridge's arm64 build is one static program, which this kernel runs)"
	fi
	say "Found $DISTRO_LINE, $ARCH$cp_note."
	if [ "$DISTRO" = volumio ]; then
		say "On Volumio, a factory reset or a system update can remove what is installed outside /data, the bridge included;" \
			"run this installer again afterwards, and pair your phone again. Your music is never touched."
	fi
}

check_user() {
	USER_ID=$(id -u)
	USER_NAME=$(id -un 2>/dev/null) || USER_NAME=$USER_ID
	if [ "$USER_ID" = 0 ]; then
		cu_sudo_user=${SUDO_USER:-}
		if [ -n "$cu_sudo_user" ] && [ "$cu_sudo_user" != root ]; then
			leave 3 "Run this installer without sudo, as $cu_sudo_user: it installs the bridge as $cu_sudo_user's own service," \
				"and asks for sudo itself for the one step that needs it. Nothing was changed."
		fi
		leave 3 "Run this installer as the user the bridge should run as, not as root: it installs the bridge as that user's own service," \
			"and asks for sudo itself for the one step that needs it." \
			"If this box has only root, add a user first (adduser music), log in as that user over SSH, and run the command again." \
			"Nothing was changed."
	fi
	[ -n "${HOME:-}" ] || leave 3 "HOME is not set, so there is nowhere to install the bridge. Nothing was changed."
	if command -v getent >/dev/null 2>&1; then
		cu_home=$(getent passwd "$USER_NAME" 2>/dev/null | cut -d: -f6) || cu_home=''
		if [ -n "$cu_home" ] && [ "${cu_home%/}" != "${HOME%/}" ]; then
			leave 3 "HOME is $HOME, but $USER_NAME's home is $cu_home: systemd looks for $USER_NAME's services in the second." \
				"Log in as $USER_NAME (not with su or sudo -E) and run this again. Nothing was changed."
		fi
	fi
}

locate_paths() {
	BIN_DIR=$HOME/.local/bin
	TARGET=$BIN_DIR/bridge
	CONFIG_DIR=${XDG_CONFIG_HOME:-$HOME/.config}/1-bit-bridge
	CONFIG_FILE=$CONFIG_DIR/bridge.yaml
	UNIT_FILE=$HOME/.config/systemd/user/$UNIT_NAME
	LOG_DIR=${XDG_STATE_HOME:-$HOME/.local/state}/1-bit-bridge
}

check_clock() {
	cc_year=$(date +%Y 2>/dev/null) || cc_year=0
	case $cc_year in
	'' | *[!0-9]*) cc_year=0 ;;
	esac
	if [ "$cc_year" -lt 2026 ]; then
		leave 3 "This box's clock says the year is $cc_year, and secure downloads fail until it is right." \
			"Check it with timedatectl (it should say the clock is synchronized), or set it: sudo date -s 'YYYY-MM-DD HH:MM'" \
			"Nothing was changed."
	fi
}

# mount_of PATH prints the type and options of the mount PATH is on, from
# /proc/mounts: the longest mount point PATH is in, the last one listed there.
mount_of() {
	awk -v path="$1" '
		function unescape(s) {
			gsub(/\\040/, " ", s); gsub(/\\011/, "\t", s); gsub(/\\134/, "\\", s)
			return s
		}
		{
			point = unescape($2)
			if (point == "/" || path == point || index(path, point "/") == 1) {
				if (length(point) >= best) { best = length(point); type = $3; options = $4 }
			}
		}
		END { if (best > 0) print type " " options }
	' "$SYSROOT/proc/mounts" 2>/dev/null
}

volatile_type() {
	case $1 in
	tmpfs | ramfs) return 0 ;;
	esac
	return 1
}

# check_storage refuses a home directory that does not outlive a reboot:
# tmpfs, or an overlay whose upper layer is (Raspberry Pi OS's Overlay File
# System), where the program, its settings and its service would be gone at
# the next boot.
check_storage() {
	cs_volatile=0
	if [ -r "$SYSROOT/proc/cmdline" ] && tr ' ' '\n' <"$SYSROOT/proc/cmdline" | grep -qx 'boot=overlay'; then
		cs_volatile=1
	fi
	if [ -r "$SYSROOT/proc/mounts" ]; then
		cs_mount=$(mount_of "$HOME")
		cs_type=${cs_mount%% *}
		cs_options=${cs_mount#* }
		if volatile_type "$cs_type"; then
			cs_volatile=1
		elif [ "$cs_type" = overlay ]; then
			cs_upper=$(printf '%s\n' "$cs_options" | tr ',' '\n' | sed -n 's/^upperdir=//p' | head -n 1)
			if [ -n "$cs_upper" ]; then
				cs_upper_mount=$(mount_of "$cs_upper")
				if volatile_type "${cs_upper_mount%% *}"; then
					cs_volatile=1
				fi
			fi
		fi
	fi
	if [ "$cs_volatile" = 1 ]; then
		leave 3 "This box keeps nothing written to $HOME through a reboot (its root is an overlay in memory, such as Raspberry Pi OS's Overlay File System):" \
			"the bridge, its settings and its service would be gone at the next boot. Turn that off (sudo raspi-config, Performance Options)," \
			"reboot, and run this again. Nothing was changed."
	fi
}

# read_unit FILE reads the binary and the config the service runs from its
# ExecStart line, which bridge init writes as ExecStart="<binary>" serve
# --config "<config>". Any other shape (an edit, a path that needed
# escaping) fails, with UNIT_PROBLEM saying why. What the service manager
# runs besides the file, its drop-ins wherever they are, is
# check_installed_unit's to read, from the manager.
read_unit() {
	ru_file=$1
	INSTALLED_BIN=''
	INSTALLED_CONFIG=''
	UNIT_PROBLEM=''
	ru_count=$(grep -c '^ExecStart=' "$ru_file" 2>/dev/null) || ru_count=0
	if [ "$ru_count" != 1 ]; then
		UNIT_PROBLEM="$ru_file has $ru_count ExecStart lines, and bridge init writes one."
		return 1
	fi
	ru_line=$(grep '^ExecStart=' "$ru_file")
	case $ru_line in
	*\\* | *%* | *'$'*)
		UNIT_PROBLEM="$ru_file runs a path with a backslash, a % or a \$ in it, which this installer does not read."
		return 1
		;;
	esac
	ru_pattern='^ExecStart="\([^"]*\)" serve --config "\([^"]*\)"$'
	INSTALLED_BIN=$(printf '%s\n' "$ru_line" | sed -n "s|$ru_pattern|\\1|p")
	INSTALLED_CONFIG=$(printf '%s\n' "$ru_line" | sed -n "s|$ru_pattern|\\2|p")
	case $INSTALLED_BIN:$INSTALLED_CONFIG in
	/*:/*) ;;
	*)
		UNIT_PROBLEM="$ru_file was changed since bridge init wrote it (its ExecStart is not the one init writes)."
		INSTALLED_BIN=''
		INSTALLED_CONFIG=''
		return 1
		;;
	esac
	return 0
}

user_bus_ok() {
	systemctl --user show-environment >/dev/null 2>&1
}

# find_user_bus makes sure systemctl --user reaches this user's service
# manager, changing nothing on the box: through the runtime directory when
# the session did not say where it is (su, a script). It fails when there is
# none to reach.
find_user_bus() {
	if user_bus_ok; then
		return 0
	fi
	fb_runtime=$SYSROOT/run/user/$USER_ID
	if [ -z "${XDG_RUNTIME_DIR:-}" ] && [ -S "$fb_runtime/bus" ]; then
		XDG_RUNTIME_DIR=$fb_runtime
		export XDG_RUNTIME_DIR
		if user_bus_ok; then
			return 0
		fi
	fi
	return 1
}

# inspect_unit asks this user's service manager for the bridge's unit, two
# ways, since they can differ. UNIT_LOADED is the unit as the manager has it
# loaded, the one a start or a restart runs (systemctl --user show): its
# LoadState, the files it was read from (FragmentPath, DropInPaths), whether
# they changed since (NeedDaemonReload), and the environment it gives the
# service. UNIT_TEXT is its files as they are on disk (systemctl --user cat,
# which reads the files, not what the manager loaded: systemctl(1)), every
# file under a comment naming it, and UNIT_FILES those files, the unit file
# first and then its drop-ins; both are empty for a unit the manager has not
# found, whose drop-ins it reads nowhere (unit_files_on_disk looks for those).
# A manager that cannot say is a refusal: what is installed cannot be told.
inspect_unit() {
	iu_status=0
	UNIT_LOADED=$(systemctl --user show \
		-p LoadState,FragmentPath,DropInPaths,NeedDaemonReload,Environment,EnvironmentFiles,PassEnvironment,UnsetEnvironment \
		"$UNIT_NAME" 2>&1) || iu_status=$?
	if [ "$iu_status" != 0 ]; then
		leave 3 "Asking $USER_NAME's service manager for the bridge's unit failed (systemctl --user show $UNIT_NAME, exit $iu_status):" \
			"$(printf '%s\n' "$UNIT_LOADED" | head -n 1)" \
			"so this installer cannot tell how the bridge is installed here. Nothing was changed."
	fi
	iu_status=0
	UNIT_TEXT=$(systemctl --user cat "$UNIT_NAME" 2>&1) || iu_status=$?
	if [ "$iu_status" != 0 ]; then
		case $UNIT_TEXT in
		*'No files found for '*)
			UNIT_TEXT=''
			UNIT_FILES=''
			return 0
			;;
		esac
		leave 3 "Asking $USER_NAME's service manager for the bridge's unit failed (systemctl --user cat $UNIT_NAME, exit $iu_status):" \
			"$(printf '%s\n' "$UNIT_TEXT" | head -n 1)" \
			"so this installer cannot tell how the bridge is installed here. Nothing was changed."
	fi
	UNIT_FILES=$(printf '%s\n' "$UNIT_TEXT" | sed -n 's|^# \(/.*\)$|\1|p')
}

# loaded PROPERTY prints the value the manager's loaded unit gives PROPERTY
# (inspect_unit), empty when it gives none.
loaded() {
	printf '%s\n' "$UNIT_LOADED" | sed -n "s/^$1=//p" | head -n 1
}

# loaded_environment prints the environment properties the manager's loaded
# unit gives the service a value for (Environment, EnvironmentFiles,
# PassEnvironment, UnsetEnvironment), comma separated: their names, never
# their values, which can hold a password.
loaded_environment() {
	le_set=''
	for le_key in Environment EnvironmentFiles PassEnvironment UnsetEnvironment; do
		if printf '%s\n' "$UNIT_LOADED" | grep -q "^$le_key=."; then
			le_set="$le_set${le_set:+, }$le_key"
		fi
	done
	printf '%s' "$le_set"
}

# unit_files_on_disk prints, one a line, the files the user's manager would
# read for the bridge's unit once there is one, wherever they are among the
# directories it reads units from (systemd-analyze --user unit-paths): a
# unit file of its name; its drop-ins, in <unit>.d and in the directories
# its dashes name (com.acoseac.1-bit-.service.d and com.acoseac.1-.service.d:
# systemd.unit(5)); and the drop-ins for every service (service.d) that set
# an environment or an ExecStart. It reads the directories themselves:
# systemctl answers "No files found" for a unit the manager has not found,
# before it reads any drop-in, so the files left for one show nowhere else
# until bridge init makes the unit and they apply to it. It fails when the
# directories cannot be listed.
unit_files_on_disk() {
	uf_dirs=$(systemd-analyze --user unit-paths 2>&1) || {
		printf '%s\n' "$uf_dirs" | head -n 1
		return 1
	}
	printf '%s\n' "$uf_dirs" | while IFS= read -r uf_dir; do
		case $uf_dir in
		/*) ;;
		*) continue ;;
		esac
		if [ -e "$uf_dir/$UNIT_NAME" ] || [ -L "$uf_dir/$UNIT_NAME" ]; then
			printf '%s\n' "$uf_dir/$UNIT_NAME"
		fi
		for uf_file in "$uf_dir/$UNIT_NAME.d"/*.conf "$uf_dir/com.acoseac.1-bit-.service.d"/*.conf \
			"$uf_dir/com.acoseac.1-.service.d"/*.conf; do
			if [ -e "$uf_file" ] || [ -L "$uf_file" ]; then
				printf '%s\n' "$uf_file"
			fi
		done
		for uf_file in "$uf_dir/service.d"/*.conf; do
			if [ -f "$uf_file" ] && grep -Eq '^[[:space:]]*(Environment|EnvironmentFile|PassEnvironment|UnsetEnvironment|ExecStart)[[:space:]]*=' "$uf_file"; then
				printf '%s\n' "$uf_file"
			fi
		done
	done
}

# unit_keys KEYS reads systemctl cat's output and prints each line of it
# that sets one of KEYS (an ERE alternation, such as
# Environment|EnvironmentFile) as the key and the file it is in: never its
# value, which can be a password.
unit_keys() {
	awk -v keys="^[ \t]*($1)[ \t]*=" '
		/^# \// { file = substr($0, 3); next }
		$0 ~ keys {
			key = $0
			sub(/^[ \t]*/, "", key)
			sub(/[ \t]*=.*/, "", key)
			print "  " key ", in " file
		}
	'
}

# check_installed_unit takes the unit at UNIT_FILE for an install only when
# it is the one bridge init writes, the manager has it loaded as its files
# are, and the program and the settings it names are there. The manager's
# loaded unit (inspect_unit) must be loaded (LoadState), from that file
# (FragmentPath), with no daemon-reload pending (NeedDaemonReload: a file
# changed since the manager read it, so a restart would run what the
# manager holds, not what the files say), and give the service no
# environment of its own (Environment, EnvironmentFiles, PassEnvironment,
# UnsetEnvironment), which bridge update, run from here, would not have:
# the bridge takes a BRIDGE_ variable over its settings. Its files, which
# are then what it loaded, must set the ExecStart once. And the program's
# directory must be one its user can write, as bridge update needs.
# Anything else is refused.
check_installed_unit() {
	read_unit "$UNIT_FILE" || leave 3 "The bridge's service is installed, but $UNIT_PROBLEM" \
		"Update the bridge with bridge update instead. Nothing was changed."
	ci_state=$(loaded LoadState)
	if [ "$ci_state" != loaded ]; then
		leave 3 "$USER_NAME's service manager does not have the bridge's unit loaded as it is (LoadState=${ci_state:-unknown}), though $UNIT_FILE is there." \
			"See why with: systemctl --user status $UNIT_NAME, then run this again. Nothing was changed."
	fi
	if [ "$(loaded NeedDaemonReload)" != no ]; then
		leave 3 "The bridge's unit changed on disk since $USER_NAME's service manager last read it (NeedDaemonReload=yes):" \
			"the manager runs it as it was then, which is not what its files say now." \
			"Have the manager read it with: systemctl --user daemon-reload, then run this again. Nothing was changed."
	fi
	ci_first=$(loaded FragmentPath)
	ci_real=$(readlink -f "$UNIT_FILE" 2>/dev/null) || ci_real=''
	if [ "$ci_first" != "$UNIT_FILE" ] && [ "$ci_first" != "$ci_real" ]; then
		leave 3 "$USER_NAME's service manager runs the bridge's service from ${ci_first:-a unit with no file}, not from $UNIT_FILE, which bridge init wrote:" \
			"this installer does not manage that unit. Nothing was changed."
	fi
	ci_set=$(loaded_environment)
	if [ -n "$ci_set" ]; then
		ci_env=$(printf '%s\n' "$UNIT_TEXT" | unit_keys 'Environment|EnvironmentFile|PassEnvironment|UnsetEnvironment')
		[ -n "$ci_env" ] || ci_env="  (in no file of the unit as it is now: the manager read them before they changed)"
		leave 3 "The bridge's service, as $USER_NAME's service manager has it loaded, has an environment of its own ($ci_set)," \
			"which bridge update, run from here, would not have (the bridge takes a BRIDGE_ variable over its settings), from:" "$ci_env" \
			"bridge init writes none. Update the bridge with its own bridge update, in that environment," \
			"or take it out (systemctl --user edit $UNIT_NAME, then systemctl --user daemon-reload) and run this again. Nothing was changed."
	fi
	ci_execs=$(printf '%s\n' "$UNIT_TEXT" | unit_keys ExecStart)
	if [ "$(printf '%s\n' "$ci_execs" | awk 'NF { n++ } END { print n + 0 }')" != 1 ]; then
		leave 3 "The bridge's service sets its ExecStart more than once, so this installer cannot tell which program it runs:" \
			"$ci_execs" "Update the bridge with bridge update instead. Nothing was changed."
	fi
	if [ ! -f "$INSTALLED_BIN" ] || [ ! -x "$INSTALLED_BIN" ]; then
		leave 3 "The bridge's service runs $INSTALLED_BIN, which is not there." \
			"Run bridge init again with the program you want it to run (it keeps the settings). Nothing was changed."
	fi
	if [ ! -f "$INSTALLED_CONFIG" ]; then
		leave 3 "The bridge's service reads $INSTALLED_CONFIG, which is not there. Run bridge init again. Nothing was changed."
	fi
	ci_bin_real=$(readlink -f "$INSTALLED_BIN" 2>/dev/null) || ci_bin_real=''
	[ -n "$ci_bin_real" ] || ci_bin_real=$INSTALLED_BIN
	ci_dir=$(dirname "$ci_bin_real")
	if [ ! -w "$ci_dir" ]; then
		leave 3 "The bridge's service runs $ci_bin_real, in $ci_dir, where $USER_NAME cannot write:" \
			"neither this installer nor bridge update can replace it there." \
			"Copy it to $BIN_DIR and run bridge init again from there (it keeps the settings), then run this again. Nothing was changed."
	fi
}

# detect_install decides, before anything changes, what this box has: the
# bridge installed as this user's service, as bridge init installs it (an
# upgrade), or nothing of the bridge at all (a new install). Anything else is
# refused: a system service; a unit the user's service manager has from
# files bridge init did not write, or runs otherwise than init wrote it
# (check_installed_unit); settings no service runs; or anything at
# ~/.local/bin/bridge that no such service runs, which only that program's
# own bridge update may replace. On a box that runs systemd the manager is
# asked first, and must answer.
detect_install() {
	INSTALL_STATE=fresh
	SYSTEMD=0
	UNIT_TEXT=''
	UNIT_FILES=''
	for di_unit in "$SYSROOT"/etc/systemd/system/*1-bit-bridge*.service "$SYSROOT"/etc/systemd/system/bridge@.service; do
		if [ -e "$di_unit" ] || [ -L "$di_unit" ]; then
			leave 3 "This box runs the bridge as a system service ($di_unit), which this installer does not manage." \
				"Update that install the way it was set up (bridge update, run as that service's user). Nothing was changed."
		fi
	done
	di_local=0
	if [ -e "$UNIT_FILE" ] || [ -L "$UNIT_FILE" ]; then
		di_local=1
	fi
	if [ -d "$SYSROOT/run/systemd/system" ]; then
		SYSTEMD=1
		if ! find_user_bus; then
			leave 3 "There is no systemd user session for $USER_NAME here (systemctl --user does not reach $USER_NAME's service manager)," \
				"so this installer can neither see what is installed as $USER_NAME's service nor install it." \
				"Log in as $USER_NAME over SSH or at the console (not with su or sudo), and run this again;" \
				"or first turn lingering on, which starts that manager, as root:" \
				"    $(command_line loginctl enable-linger "$USER_NAME")" \
				"Nothing was changed."
		fi
		inspect_unit
	elif [ "$di_local" = 1 ]; then
		leave 3 "The bridge's service unit is at $UNIT_FILE, but this system is not running systemd," \
			"so this installer can neither ask how it runs nor restart it. Nothing was changed."
	fi
	if [ "$di_local" = 1 ]; then
		check_installed_unit
		INSTALL_STATE=upgrade
		return 0
	fi
	if [ "$SYSTEMD" = 1 ]; then
		di_state=$(loaded LoadState)
		if [ "$di_state" != not-found ] || [ -n "$UNIT_FILES" ] || [ -n "$UNIT_TEXT" ]; then
			leave 3 "$USER_NAME's service manager has the bridge's unit, $UNIT_NAME (LoadState=${di_state:-unknown}), from files bridge init did not write:" \
				"$(printf '%s\n' "${UNIT_FILES:-$(loaded FragmentPath) $(loaded DropInPaths)}" | sed 's/^/  /')" \
				"bridge init writes it at $UNIT_FILE alone. Update that install the way it was set up (bridge update). Nothing was changed."
		fi
		di_left=$(unit_files_on_disk) ||
			leave 3 "Listing the directories $USER_NAME's service manager reads units from failed (systemd-analyze --user unit-paths): $di_left" \
				"so this installer cannot tell whether files are left there for the bridge's unit. Nothing was changed."
		if [ -n "$di_left" ]; then
			leave 3 "$USER_NAME's service manager has no unit for the bridge, but would read these files for one once bridge init makes it:" \
				"$(printf '%s\n' "$di_left" | sed 's/^/  /')" \
				"so the service bridge init installs would run with what they set. Move them aside, then run this again. Nothing was changed."
		fi
	fi
	if [ -e "$CONFIG_FILE" ]; then
		leave 3 "Found the bridge's settings at $CONFIG_FILE, but no service runs them, so this installer cannot tell which bridge program you use." \
			"If you start it yourself, update it with: bridge update" \
			"To run it as a service, run bridge init with that program (it keeps the settings)." \
			"For a new install, move $CONFIG_DIR aside first. Nothing was changed."
	fi
	if [ -e "$TARGET" ] || [ -L "$TARGET" ]; then
		leave 3 "$TARGET is there, and the bridge is not installed as $USER_NAME's service (there is no $UNIT_FILE)," \
			"so this installer cannot tell what runs it, and does not replace it: only a bridge's own bridge update does." \
			"If it is a bridge you run yourself, update it with its own bridge update (naming the settings it serves, if not these):" \
			"    $(bridge_line "$TARGET" update --config "$CONFIG_FILE")" \
			"or make it your service, with:" \
			"    $(bridge_line "$TARGET" init)" \
			"If it is something else, move it aside and run this again. Nothing was changed."
	fi
}

# check_environment refuses, before anything changes, a run whose bridge
# would read other settings than the service does. The bridge takes a
# BRIDGE_ variable over its bridge.yaml, --config or not, and bridge init,
# bridge update and bridge status, run here, take this shell's, where the
# service takes its manager's: so a BRIDGE_ variable this shell exports, or
# one the manager gives every service of this user, is refused, by its name,
# never its value. env prints a value's newlines as they are, so a value
# holding a line that starts BRIDGE_ reads as a variable too: a refusal,
# never a miss. BRIDGE_INSTALL_* are this installer's own, and the bridge
# reads none of them.
check_environment() {
	ce_here=$(env | sed -n -e '/^BRIDGE_INSTALL_/d' -e 's/^\(BRIDGE_[A-Za-z0-9_]*\)=.*/\1/p' | sort -u | tr '\n' ' ')
	if [ -n "$ce_here" ]; then
		leave 3 "This shell exports ${ce_here% }, which the bridge's service would not have: the bridge takes a BRIDGE_ variable over its settings," \
			"so what this installer runs would read other settings than the service does." \
			"Run it again from a shell without them (unset ${ce_here% }). Nothing was changed."
	fi
	ce_manager=$(systemctl --user show-environment 2>/dev/null | sed -n -e '/^BRIDGE_INSTALL_/d' -e 's/^\(BRIDGE_[A-Za-z0-9_]*\)=.*/\1/p' | sort -u | tr '\n' ' ')
	if [ -n "$ce_manager" ]; then
		leave 3 "$USER_NAME's service manager gives every service of $USER_NAME ${ce_manager% } (systemctl --user show-environment)," \
			"which bridge init and bridge update, run from here, would not have: the service would read other settings than they do." \
			"Take it out (systemctl --user unset-environment, and where it is set, such as ~/.config/environment.d), then run this again." \
			"Nothing was changed."
	fi
}

# open_terminal opens the terminal on file descriptor 3 when there is one,
# for the questions this installer asks and for the commands that ask their
# own (bridge init, sudo, su): under curl | sh, standard input is the script.
open_terminal() {
	INTERACTIVE=0
	# A failed redirection of exec ends the shell, so the open is tried in a
	# subshell first.
	if (exec 3<"$TTY_DEVICE") 2>/dev/null; then
		exec 3<"$TTY_DEVICE"
		INTERACTIVE=1
	fi
}

make_temp_dir() {
	command -v mktemp >/dev/null 2>&1 || leave 3 "This installer needs mktemp. Nothing was changed."
	TMP_DIR=$( (umask 077 && mktemp -d "${TMPDIR:-/tmp}/1-bit-bridge-install.XXXXXX") 2>/dev/null) ||
		leave 1 "Could not create a temporary directory in ${TMPDIR:-/tmp}. Nothing was changed."
}

# check_tools makes sure of the tools the download needs, refusing before
# any download when one is missing. curl alone downloads, held to HTTPS
# through every redirect (fetch): GNU wget follows a redirect from HTTPS to
# plain HTTP, and BusyBox's does not check the server's certificate, so
# wget is never used in curl's place.
check_tools() {
	ct_missing=''
	command -v tar >/dev/null 2>&1 || ct_missing="$ct_missing tar"
	command -v curl >/dev/null 2>&1 || ct_missing="$ct_missing curl"
	if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
		ct_missing="$ct_missing sha256sum"
	fi
	if [ -n "$ct_missing" ]; then
		ct_why=''
		case " $ct_missing " in
		*' curl '*) ct_why=' It downloads with curl alone, which it holds to HTTPS through every redirect; wget is not used in its place.' ;;
		esac
		leave 3 "This installer needs:$ct_missing.$ct_why" \
			"On Debian, Ubuntu and Raspberry Pi OS, install them with:" \
			"    sudo apt install curl coreutils tar" \
			"and run it again. Nothing was changed."
	fi
}

# fetch URL FILE downloads over HTTPS only, TLS 1.2 or later, every redirect
# included.
fetch() {
	curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --retry 3 --connect-timeout 30 -o "$2" "$1"
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	else
		shasum -a 256 "$1" | awk '{ print $1 }'
	fi
}

# expected_sum CHECKSUMS NAME prints the SHA-256 checksums.txt gives NAME: the
# one line whose file name is NAME exactly, its hash 64 hex digits. Anything
# else, no line or two, prints nothing.
expected_sum() {
	es_sum=$(awk -v name="$2" '$2 == name { n++; sum = $1 } END { if (n == 1) print sum }' "$1" | tr '[:upper:]' '[:lower:]')
	case $es_sum in
	'' | *[!0-9a-f]*) return 0 ;;
	esac
	if [ "${#es_sum}" -eq 64 ]; then
		printf '%s' "$es_sum"
	fi
}

# version_from_text reads what `bridge version` prints, "1-bit-bridge 0.2.2
# (protocol v1)", and prints the release, without a v.
version_from_text() {
	vft_version=$(sed -n 's/^1-bit-bridge \([^ ]*\).*/\1/p' | head -n 1)
	printf '%s' "${vft_version#v}"
}

# status_field LABEL reads what `bridge status` prints and prints the first
# word of the row LABEL names ("Server version", "Admin address").
status_field() {
	sed -n "s/^$1: *\\([^ ]*\\).*/\\1/p" | head -n 1
}

# bridge_version BINARY prints the release BINARY says it is, without a v.
bridge_version() {
	bv_out=$("$1" version </dev/null 2>/dev/null) || return 1
	bv_version=$(printf '%s\n' "$bv_out" | version_from_text)
	[ -n "$bv_version" ] || return 1
	printf '%s' "$bv_version"
}

resolve_version() {
	if [ -n "$OPT_VERSION" ]; then
		VERSION=$OPT_VERSION
		return 0
	fi
	if [ -n "$STAMPED_VERSION" ]; then
		VERSION=$(normalize_version "$STAMPED_VERSION") ||
			leave 1 "This installer was published with a release it cannot read ($STAMPED_VERSION). Nothing was changed."
		return 0
	fi
	leave 2 "This copy of the installer is not from a release, so it does not know which release to install." \
		"Name one with --version X.Y.Z (the releases are at $REPO_URL/releases). Nothing was changed."
}

usable_folder() {
	[ -d "$1" ] && [ -r "$1" ] && [ -x "$1" ]
}

# folder_is_empty DIR tells whether DIR holds no entry, dot entries included.
folder_is_empty() {
	for fe_entry in "$1"/* "$1"/.[!.]* "$1"/..?*; do
		if [ -e "$fe_entry" ] || [ -L "$fe_entry" ]; then
			return 1
		fi
	done
	return 0
}

# choose_library offers the music folders this kind of box keeps its music in
# that are here (Volumio's and moOde's own, and ~/Music), or asks for one,
# and sets LIBRARY to the answer. Without a terminal it asks nothing and only
# sets FOUND_FOLDERS, for the refusal that says how to give one.
choose_library() {
	case $DISTRO in
	volumio) set -- /mnt/USB /mnt/NAS /data/INTERNAL ;;
	moode) set -- /mnt/SDCARD /media /mnt/NAS ;;
	*) set -- ;;
	esac
	# Keep the ones that are here: each is taken off the front and put back
	# at the end when it is usable.
	cl_left=$#
	while [ "$cl_left" -gt 0 ]; do
		cl_dir=$SYSROOT$1
		shift
		if usable_folder "$cl_dir"; then
			set -- "$@" "$cl_dir"
		fi
		cl_left=$((cl_left - 1))
	done
	if usable_folder "$HOME/Music"; then
		set -- "$@" "$HOME/Music"
	fi
	FOUND_FOLDERS=$*
	[ "$INTERACTIVE" = 1 ] || return 0
	if [ "$#" -eq 0 ]; then
		ask_library_path
		return 0
	fi
	warn "" "Which folder holds your music? (another one can be added later, in the bridge's console)"
	cl_count=0
	for cl_dir in "$@"; do
		cl_count=$((cl_count + 1))
		cl_note=''
		if folder_is_empty "$cl_dir"; then
			cl_note=' (empty now)'
		fi
		warn "  $cl_count) $cl_dir$cl_note"
	done
	cl_other=$((cl_count + 1))
	warn "  $cl_other) another folder"
	cl_try=0
	while [ "$cl_try" -lt 3 ]; do
		cl_try=$((cl_try + 1))
		printf 'Choose [1]: ' >&2
		IFS= read -r cl_answer <&3 || leave 1 "" "No answer; nothing was installed."
		case $cl_answer in
		'') cl_answer=1 ;;
		*[!0-9]*) cl_answer=0 ;;
		esac
		if [ "$cl_answer" -eq "$cl_other" ]; then
			ask_library_path
			return 0
		fi
		if [ "$cl_answer" -ge 1 ] && [ "$cl_answer" -le "$cl_count" ]; then
			cl_index=0
			for cl_dir in "$@"; do
				cl_index=$((cl_index + 1))
				if [ "$cl_index" -eq "$cl_answer" ]; then
					LIBRARY=$cl_dir
				fi
			done
			return 0
		fi
		warn "Choose a number from 1 to $cl_other."
	done
	leave 1 "No music folder chosen; nothing was installed." \
		"Run this again with --library /path/to/music, or with --no-init to install the program alone."
}

ask_library_path() {
	alp_try=0
	while [ "$alp_try" -lt 3 ]; do
		alp_try=$((alp_try + 1))
		printf 'Music folder (its full path): ' >&2
		IFS= read -r alp_answer <&3 || leave 1 "" "No answer; nothing was installed."
		case $alp_answer in
		'~') alp_answer=$HOME ;;
		'~'/*) alp_answer=$HOME/${alp_answer#'~'/} ;;
		esac
		[ -n "$alp_answer" ] || continue
		case $alp_answer in
		/*) ;;
		*) alp_answer=$PWD/$alp_answer ;;
		esac
		if [ -d "$alp_answer" ]; then
			LIBRARY=$alp_answer
			return 0
		fi
		warn "$alp_answer is not a folder."
	done
	leave 1 "No music folder chosen; nothing was installed." \
		"Run this again with --library /path/to/music, or with --no-init to install the program alone."
}

# download_release downloads the archive for this box and the release's
# checksums.txt from the release's own tag, and unpacks the bridge program
# into the temporary directory once its SHA-256 matches. Nothing is
# installed yet.
download_release() {
	ARCHIVE=1-bit-bridge_${VERSION}_linux_${ARCH}.tar.gz
	dr_url=$DOWNLOAD_BASE/v$VERSION
	say "Downloading the bridge $VERSION for linux/$ARCH from $dr_url/"
	fetch "$dr_url/checksums.txt" "$TMP_DIR/checksums.txt" ||
		leave 1 "Could not download $dr_url/checksums.txt: is $VERSION a published release? Nothing was installed."
	fetch "$dr_url/$ARCHIVE" "$TMP_DIR/$ARCHIVE" ||
		leave 1 "Could not download $dr_url/$ARCHIVE. Nothing was installed."
	dr_want=$(expected_sum "$TMP_DIR/checksums.txt" "$ARCHIVE")
	[ -n "$dr_want" ] ||
		leave 1 "The release's checksums.txt does not give $ARCHIVE one SHA-256, so the download cannot be checked. Nothing was installed."
	dr_got=$(sha256_of "$TMP_DIR/$ARCHIVE" | tr '[:upper:]' '[:lower:]')
	if [ "$dr_got" != "$dr_want" ]; then
		leave 1 "The download's SHA-256 is $dr_got, and the release's checksums.txt says $dr_want:" \
			"it is not the release's archive. Nothing was installed."
	fi
	say "Its SHA-256 matches the release's checksums.txt."
	mkdir "$TMP_DIR/unpacked"
	tar -xzf "$TMP_DIR/$ARCHIVE" -C "$TMP_DIR/unpacked" bridge 2>"$TMP_DIR/tar.err" ||
		leave 1 "Could not unpack the bridge from $ARCHIVE: $(cat "$TMP_DIR/tar.err") Nothing was installed."
	if [ ! -f "$TMP_DIR/unpacked/bridge" ] || [ -L "$TMP_DIR/unpacked/bridge" ]; then
		leave 1 "$ARCHIVE holds no bridge program. Nothing was installed."
	fi
}

# install_binary puts the program in place in one rename: it is copied beside
# the target first and checked to run as the release it should be, then
# renamed into place, so the target is never a partial file. Nothing was at
# the target (detect_install refuses otherwise).
install_binary() {
	if [ ! -d "$BIN_DIR" ]; then
		if [ ! -d "$HOME/.local" ]; then
			CREATED_LOCAL=$HOME/.local
		fi
		CREATED_BIN=$BIN_DIR
		(umask 022 && mkdir -p "$BIN_DIR") || leave 1 "Could not create $BIN_DIR. Nothing was installed."
	fi
	STAGE=$BIN_DIR/.bridge.install.$$
	cp "$TMP_DIR/unpacked/bridge" "$STAGE" || leave 1 "Could not write $STAGE. Nothing was installed."
	chmod 0755 "$STAGE" || leave 1 "Could not make $STAGE executable. Nothing was installed."
	ib_version=$(bridge_version "$STAGE") ||
		leave 1 "The downloaded bridge does not run on this system (linux/$ARCH). Nothing was installed."
	if [ "$ib_version" != "$VERSION" ]; then
		leave 1 "The downloaded program says it is the bridge $ib_version, not $VERSION. Nothing was installed."
	fi
	mv -f "$STAGE" "$TARGET" || leave 1 "Could not move the bridge into place at $TARGET. Nothing was installed."
	STAGE=''
	INSTALLED=1
	say "Installed the bridge $VERSION at $TARGET."
}

linger_on() {
	loginctl show-user "$USER_NAME" --property=Linger 2>/dev/null | grep -qx 'Linger=yes'
}

safe_user_name() {
	case $USER_NAME in
	'' | -* | *[!A-Za-z0-9._-]*) return 1 ;;
	esac
	return 0
}

# ensure_linger turns on systemd's lingering for this user, so that the user
# manager, and the bridge's service with it, starts at boot and outlives the
# login. It is the one step that may take root, and it says so first. LINGER
# is what lingering is once it has run: on (it was, or it is now), off, or
# none where there is no loginctl to turn it on.
ensure_linger() {
	LINGER=none
	if ! command -v loginctl >/dev/null 2>&1; then
		warn "There is no loginctl here, so this installer cannot turn lingering on, which keeps the service running after you log out."
		return 0
	fi
	LINGER=on
	if linger_on; then
		return 0
	fi
	say "" "So that the bridge keeps running after you log out and starts when this box boots, systemd keeps" \
		"$USER_NAME's services running (lingering: loginctl enable-linger $USER_NAME)."
	if loginctl --no-ask-password enable-linger >/dev/null 2>&1 && linger_on; then
		say "Lingering is on for $USER_NAME."
		return 0
	fi
	el_now=0
	if command -v sudo >/dev/null 2>&1; then
		say "That needs administrator rights: sudo runs loginctl enable-linger $USER_NAME, and may ask for your password."
		if [ "$INTERACTIVE" = 1 ]; then
			if sudo loginctl enable-linger "$USER_NAME" <&3 && linger_on; then
				el_now=1
			fi
		elif sudo -n loginctl enable-linger "$USER_NAME" </dev/null && linger_on; then
			el_now=1
		fi
	elif [ "$INTERACTIVE" = 1 ] && command -v su >/dev/null 2>&1 && safe_user_name; then
		say "There is no sudo here: su runs loginctl enable-linger $USER_NAME as root, and asks for root's password."
		if su root -c "loginctl enable-linger $USER_NAME" <&3 && linger_on; then
			el_now=1
		fi
	fi
	if [ "$el_now" = 1 ]; then
		say "Lingering is on for $USER_NAME."
		return 0
	fi
	LINGER=off
	warn "Lingering is not on, so the bridge stops when you log out, and starts only when you log in. To turn it on, run as root:" \
		"    $(command_line loginctl enable-linger "$USER_NAME")"
}

# say_running says what runs now and for how long: the service, and whether
# it outlives the login and starts at boot, which is lingering's
# (ensure_linger).
say_running() {
	case $LINGER in
	on)
		say "" "The bridge $SERVED is running as $USER_NAME's service, and starts again when this box boots."
		;;
	off)
		say "" "The bridge $SERVED is running as $USER_NAME's service, until $USER_NAME's last login session on this box ends: lingering is not on," \
			"so it stops then, and does not start when this box boots (above is how to turn it on)."
		;;
	*)
		say "" "The bridge $SERVED is running as $USER_NAME's service. This box has no loginctl, so lingering is not on:" \
			"the service may stop when you log out, and not start when this box boots."
		;;
	esac
}

# shown_bridge prints how to name the program at BINARY in a command the user
# types: plain bridge only where the PATH finds this one (on Ubuntu, bridge
# is also iproute2's program in /usr/sbin), its path quoted otherwise.
shown_bridge() {
	if [ "$(command -v bridge 2>/dev/null)" = "$1" ]; then
		printf 'bridge'
	else
		sh_quote "$1"
	fi
}

path_note() {
	if [ "$(command -v bridge 2>/dev/null)" != "$TARGET" ]; then
		say "($BIN_DIR is on your PATH from your next login; until then, name the bridge by its full path, as above.)"
	fi
}

defer_setup() {
	say "" "The bridge $VERSION is installed at $TARGET, but not set up: $1"
	shift
	say "$@"
	path_note
	exit 4
}

run_init() {
	say "" "Setting the bridge up: bridge init writes its settings and its TLS identity, and installs its service."
	ri_status=0
	if [ "$INTERACTIVE" = 1 ]; then
		"$TARGET" init --library "$LIBRARY" <&3 || ri_status=$?
	else
		"$TARGET" init --yes --library "$LIBRARY" </dev/null || ri_status=$?
	fi
	if [ "$ri_status" != 0 ]; then
		leave 1 "" "bridge init stopped (exit $ri_status). The bridge is installed at $TARGET, and not set up;" \
			"fix what init says above, then set it up with:" \
			"    $(bridge_line "$TARGET" init --library "$LIBRARY")"
	fi
}

# wait_served BINARY CONFIG VERSION waits up to 20 seconds for the running
# bridge to say, through bridge status, that it serves VERSION. SERVED and
# ADMIN_ADDR are what it said last.
wait_served() {
	ws_tries=0
	SERVED=''
	ADMIN_ADDR=''
	while [ "$ws_tries" -lt 20 ]; do
		ws_out=$("$1" status --config "$2" </dev/null 2>/dev/null) || ws_out=''
		SERVED=$(printf '%s\n' "$ws_out" | status_field 'Server version')
		SERVED=${SERVED#v}
		ADMIN_ADDR=$(printf '%s\n' "$ws_out" | status_field 'Admin address')
		if [ -n "$SERVED" ] && [ "$SERVED" = "$3" ]; then
			return 0
		fi
		ws_tries=$((ws_tries + 1))
		sleep 1
	done
	return 1
}

# check_served BINARY CONFIG VERSION makes sure the service runs and serves
# VERSION. A service that runs but whose release bridge status cannot read
# is not taken for one that serves it: the run ends incomplete (exit 4),
# with the command that checks it.
check_served() {
	if ! systemctl --user is-active --quiet "$UNIT_NAME"; then
		say "The bridge's service is not running; starting it (bridge start)."
		"$1" start </dev/null || leave 1 "bridge start failed (above). See what the bridge logged:" "    $(bridge_line "$1" logs)"
	fi
	if wait_served "$1" "$2" "$3"; then
		return 0
	fi
	if [ -n "$SERVED" ]; then
		leave 1 "The service still serves the bridge $SERVED, not $3, 20 seconds on." \
			"After an update, the previous program is beside the new one, as $1.bak. See what the bridge logged:" \
			"    $(bridge_line "$1" logs)"
	fi
	if systemctl --user is-active --quiet "$UNIT_NAME"; then
		warn "" "The bridge's service is running, but bridge status could not say which release it serves, so this installer cannot tell whether it runs the bridge $3." \
			"Check it with:" "    $(bridge_line "$1" status --config "$2")"
		exit 4
	fi
	leave 1 "The bridge's service is not running. See what it logged:" "    $(bridge_line "$1" logs)"
}

report_outputs() {
	say "" "This box as a player (bridge output player):"
	"$TARGET" output player </dev/null || say "  (bridge output player could not read it: exit $?)"
	say "" "Its sound cards (bridge output dac):"
	"$TARGET" output dac </dev/null || say "  (bridge output dac could not read them: exit $?)"
}

# box_address prints this box's LAN address: the source of its route to the
# internet, found without sending anything.
box_address() {
	ba_address=''
	if command -v ip >/dev/null 2>&1; then
		ba_address=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -n 1)
	fi
	if [ -z "$ba_address" ] && command -v hostname >/dev/null 2>&1; then
		ba_address=$(hostname -I 2>/dev/null | awk '{ print $1 }')
	fi
	printf '%s' "${ba_address:-THIS-BOX}"
}

# print_pairing BINARY CONFIG says how to pair a phone with a headless box:
# the console is on loopback only, so through an SSH tunnel whose local port
# is the console's own, or bridge pair in this terminal.
print_pairing() {
	pp_address=${ADMIN_ADDR:-127.0.0.1:7789}
	pp_port=${pp_address##*:}
	pp_host=${pp_address%:*}
	case $pp_host in
	'' | 0.0.0.0 | '[::]') pp_host=127.0.0.1 ;;
	esac
	say "" "Pair your iPhone with the 1-bit app:" \
		"  The bridge's console is reachable on this box only, at http://$pp_host:$pp_port/" \
		"  From a computer on your network, reach it through SSH (the local port must be $pp_port too):" \
		"    $(command_line ssh -L "$pp_port:$pp_host:$pp_port" "$USER_NAME@$(box_address)")" \
		"  then open http://$pp_host:$pp_port/ on that computer, choose Devices, then Pair new device, and scan the QR code with the app." \
		"  Or pair it in this terminal:" \
		"    $(bridge_line "$1" pair --name 'My iPhone' --config "$2")" \
		"  and enter the token it prints in the app, with the address and the fingerprint this shows:" \
		"    $(bridge_line "$1" status --config "$2")"
}

fresh_install() {
	resolve_version
	check_tools
	SETUP=yes
	if [ "$OPT_NO_INIT" = 1 ]; then
		SETUP=no
	elif [ "$SYSTEMD" = 0 ]; then
		SETUP=nosystemd
	fi
	LIBRARY=$OPT_LIBRARY
	FOUND_FOLDERS=''
	if [ "$SETUP" = yes ] && [ -z "$LIBRARY" ]; then
		choose_library
	fi
	if [ "$SETUP" = yes ] && [ -z "$LIBRARY" ]; then
		# Refused before anything is downloaded: a program left behind would
		# make the run that names a folder refuse it.
		if [ -n "$FOUND_FOLDERS" ]; then
			leave 3 "No music folder was given (--library), and there is no terminal to ask for one. Nothing was changed." \
				"Music folders found here: $FOUND_FOLDERS" \
				"Give the one to serve with --library, as in:" \
				"    curl -fsSL https://1-bit.app/install.sh | sh -s -- --library /path/to/music"
		fi
		leave 3 "No music folder was given (--library), and there is no terminal to ask for one. Nothing was changed." \
			"Give the one to serve with --library, as in:" \
			"    curl -fsSL https://1-bit.app/install.sh | sh -s -- --library /path/to/music"
	fi
	download_release
	install_binary
	if [ "$SETUP" = no ]; then
		say "" "Set it up when you are ready, with:" "    $(bridge_line "$TARGET" init)"
		if command -v loginctl >/dev/null 2>&1 && ! linger_on; then
			say "and so that its service outlives your login and starts when this box boots, turn lingering on, as root:" \
				"    $(command_line loginctl enable-linger "$USER_NAME")"
		fi
		path_note
		return 0
	fi
	if [ "$SETUP" = nosystemd ]; then
		defer_setup "this system does not run systemd, so the bridge cannot be installed as a service here." \
			"To run it yourself:" "    $(bridge_line "$TARGET" init --no-service --library /path/to/music)" \
			"and then:" "    $(bridge_line "$TARGET" serve --config "$CONFIG_FILE")"
	fi
	ensure_linger
	run_init
	read_unit "$UNIT_FILE" || leave 1 "bridge init finished, but $UNIT_PROBLEM"
	check_served "$INSTALLED_BIN" "$INSTALLED_CONFIG" "$VERSION"
	report_outputs
	say_running
	print_pairing "$INSTALLED_BIN" "$INSTALLED_CONFIG"
	path_note
}

# run_update upgrades the installed bridge with its own bridge update: the
# check of the paired devices' versions, the swap that keeps the previous
# program as .bak, the rollback marker and the restart are all its own.
run_update() {
	ru_bin=$1
	ru_config=$2
	ru_before=$3
	say "" "Updating with bridge update, which checks your paired devices before it installs anything:"
	# The pipeline runs this group in a subshell of its own, so its status
	# comes back through a file.
	{
		ru_code=0
		"$ru_bin" update --yes --config "$ru_config" </dev/null || ru_code=$?
		printf '%s\n' "$ru_code" >"$TMP_DIR/update.status"
	} 2>&1 | tee "$TMP_DIR/update.log"
	ru_status=$(cat "$TMP_DIR/update.status" 2>/dev/null) || ru_status=1
	ru_after=$(bridge_version "$ru_bin") || ru_after=''
	if [ "$ru_after" = "$ru_before" ]; then
		if [ "$ru_status" != 0 ]; then
			leave 1 "" "bridge update stopped (exit $ru_status), and the bridge is still $ru_before: nothing was changed." \
				"What it says above is what to do."
		fi
		if grep -qF "You're already running the latest release." "$TMP_DIR/update.log"; then
			say "The bridge $ru_before is the latest release."
			return 0
		fi
		leave 1 "" "bridge update could not find out about a new release (above), and the bridge is still $ru_before:" \
			"nothing was changed. Try again later."
	fi
	if [ -z "$ru_after" ]; then
		leave 1 "" "After bridge update, $ru_bin does not run. Its previous program is kept beside it, as $ru_bin.bak."
	fi
	if [ "$ru_status" != 0 ]; then
		leave 1 "" "bridge update installed $ru_after, then stopped (exit $ru_status, above). Restart the service to run it:" \
			"    $(command_line systemctl --user restart "$UNIT_NAME")"
	fi
	say "Updated the bridge from $ru_before to $ru_after."
}

upgrade() {
	up_bin=$INSTALLED_BIN
	up_config=$INSTALLED_CONFIG
	up_before=$(bridge_version "$up_bin") ||
		leave 3 "The bridge's service runs $up_bin, which does not run here (bridge version fails). Nothing was changed."
	say "Found the bridge $up_before, installed as $USER_NAME's service: $up_bin, with its settings in $up_config."
	if [ -n "$OPT_LIBRARY" ]; then
		warn "--library is for a new install, and this bridge keeps its music folders: add one in its console instead."
	fi
	up_update=1
	if [ -n "$OPT_VERSION" ]; then
		if [ "$OPT_VERSION" != "$up_before" ]; then
			leave 3 "This box runs the bridge $up_before. An installed bridge moves only to the latest release, through bridge update," \
				"which checks your paired devices first; this installer does not install another release over it. Nothing was changed."
		fi
		up_update=0
		say "It is $OPT_VERSION already."
	fi
	ensure_linger
	if [ "$up_update" = 1 ]; then
		run_update "$up_bin" "$up_config" "$up_before"
	fi
	up_now=$(bridge_version "$up_bin") || leave 1 "$up_bin does not run any more; its previous program is beside it, as $up_bin.bak."
	check_served "$up_bin" "$up_config" "$up_now"
	say_running
	print_pairing "$up_bin" "$up_config"
}

# sh_quote VALUE prints VALUE as one word for a shell command line.
sh_quote() {
	case $1 in
	'' | *[!A-Za-z0-9_./:@=+-]*)
		printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
		;;
	*)
		printf '%s' "$1"
		;;
	esac
}

# command_line WORD... prints WORD... as a command line a POSIX shell reads
# back as those words, each quoted where it needs it (sh_quote).
command_line() {
	cmdl_line=''
	for cmdl_word in "$@"; do
		cmdl_line="$cmdl_line${cmdl_line:+ }$(sh_quote "$cmdl_word")"
	done
	printf '%s' "$cmdl_line"
}

# bridge_line BINARY ARG... prints the command that runs the bridge at BINARY
# with ARG..., for the user to type: the program as their PATH names it
# (shown_bridge), and every word quoted where it needs it. Every --config is
# kept, the default settings file's too: a bridge command without one reads
# ./bridge.yaml first where it is run (cmd/bridge's resolveConfigPath), so
# one pasted in another bridge's folder would act on that bridge's settings
# and its token store.
bridge_line() {
	bl_line=$(shown_bridge "$1")
	shift
	for bl_word in "$@"; do
		bl_line="$bl_line $(sh_quote "$bl_word")"
	done
	printf '%s' "$bl_line"
}

# print_uninstall prints the commands that remove the bridge, in the order
# they must run, and changes nothing. The music is never among them.
print_uninstall() {
	pu_found=0
	for pu_unit in "$SYSROOT"/etc/systemd/system/*1-bit-bridge*.service "$SYSROOT"/etc/systemd/system/bridge@.service; do
		if [ -e "$pu_unit" ] || [ -L "$pu_unit" ]; then
			pu_found=1
			say "A system service runs the bridge on this box ($pu_unit), and this installer did not install it:" \
				"remove it as root, the way it was set up."
		fi
	done
	INSTALLED_BIN=''
	if [ -e "$UNIT_FILE" ] || [ -L "$UNIT_FILE" ]; then
		pu_found=1
		read_unit "$UNIT_FILE" || INSTALLED_BIN=''
		say "" "To remove the bridge's service, run these as $USER_NAME, in this order:" \
			"    $(command_line systemctl --user disable --now "$UNIT_NAME")" \
			"    $(command_line systemctl --user is-active "$UNIT_NAME")    # go on only once this says inactive" \
			"    $(command_line rm "$UNIT_FILE")" \
			"    $(command_line systemctl --user daemon-reload)"
		if [ -n "$INSTALLED_BIN" ]; then
			say "    $(command_line rm -f "$INSTALLED_BIN" "$INSTALLED_BIN.bak")"
			pu_real=$(readlink -f "$INSTALLED_BIN" 2>/dev/null) || pu_real=''
			if [ -n "$pu_real" ] && [ "$pu_real" != "$INSTALLED_BIN" ]; then
				say "    $(command_line rm -f "$pu_real" "$pu_real.bak")    # the program the link above leads to"
			fi
		else
			say "  and remove the program the service's ExecStart line names, with its .bak beside it"
		fi
	fi
	if { [ -e "$TARGET" ] || [ -L "$TARGET" ]; } && [ "$TARGET" != "$INSTALLED_BIN" ]; then
		pu_found=1
		say "" "To remove the bridge program at $TARGET:" "    $(command_line rm -f "$TARGET" "$TARGET.bak")"
	fi
	if [ -e "$CONFIG_DIR" ]; then
		pu_found=1
		say "" "That leaves your music, and the bridge's settings and data in $CONFIG_DIR: its identity, the phones" \
			"paired with it and its library index. To remove those too, once the service is gone (a phone pairs" \
			"again after a reinstall):" "    $(command_line rm -rf "$CONFIG_DIR")"
	fi
	if [ -e "$LOG_DIR" ]; then
		say "" "Its log:" "    $(command_line rm -rf "$LOG_DIR")"
	fi
	if [ "$pu_found" = 0 ]; then
		say "The bridge is not installed for $USER_NAME here: there is nothing to remove."
		return 0
	fi
	say "" "Lingering is left as it is, since other services of yours may need it (loginctl disable-linger turns it off for all of them)." \
		"This installer removed nothing."
}

main() {
	set -eu
	constants
	parse_args "$@"
	setup_cleanup
	if [ -n "$STAMPED_VERSION" ]; then
		say "1-bit Bridge installer, from release $STAMPED_VERSION"
	else
		say "1-bit Bridge installer (a copy that is not from a release)"
	fi
	check_platform
	check_user
	locate_paths
	if [ "$OPT_UNINSTALL" = 1 ]; then
		print_uninstall
		return 0
	fi
	check_clock
	check_storage
	detect_install
	if [ "$INSTALL_STATE" = upgrade ] && [ "$OPT_NO_INIT" = 1 ]; then
		leave 3 "The bridge is installed here, as $USER_NAME's service ($INSTALLED_BIN, with its settings in $INSTALLED_CONFIG):" \
			"--no-init is for a new install, and an installed bridge is upgraded through its own bridge update." \
			"Run this installer without --no-init, or run:" \
			"    $(bridge_line "$INSTALLED_BIN" update --config "$INSTALLED_CONFIG")" \
			"Nothing was changed."
	fi
	if [ "$SYSTEMD" = 1 ] && [ "$OPT_NO_INIT" = 0 ]; then
		check_environment
	fi
	open_terminal
	make_temp_dir
	if [ "$INSTALL_STATE" = upgrade ]; then
		upgrade
	else
		fresh_install
	fi
	cleanup
}

# The only command at the top level, and the last line: a compound command, so
# a script cut short before its closing brace is a syntax error and runs
# nothing. (A bare `main "$@"` would not do: its prefix `main` runs.)
{ [ -n "${BRIDGE_INSTALL_TEST_SOURCE:-}" ] || main "$@"; }
