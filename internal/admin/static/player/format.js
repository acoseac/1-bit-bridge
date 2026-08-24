// Display formatting and the client half of the playability decision.

/** mm:ss, or h:mm:ss past an hour. Non-finite input renders nothing. */
export function duration(seconds) {
  if (!Number.isFinite(seconds) || seconds <= 0) return "";
  const s = Math.round(seconds);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = String(s % 60).padStart(2, "0");
  return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${sec}` : `${m}:${sec}`;
}

export function totalDuration(seconds) {
  if (!Number.isFinite(seconds) || seconds <= 0) return "";
  const mins = Math.round(seconds / 60);
  if (mins < 60) return `${mins} min`;
  const h = Math.floor(mins / 60);
  const m = mins % 60;
  return m ? `${h} h ${m} min` : `${h} h`;
}

const QUALITY_LABELS = {
  cdQuality: "CD",
  hiresPCM: "Hi-Res",
  lossy: "Lossy",
  dsd64: "DSD64",
  dsd128: "DSD128",
  dsd256Plus: "DSD256+",
  dsdUnknownRate: "DSD",
  unknown: "",
};

/** "1 album" / "2 albums" — English -s pluralisation, which covers
 * every noun the player counts (album, track, disc, mix). */
export function plural(n, word) {
  return `${n} ${n === 1 ? word : word + "s"}`;
}

export function qualityLabel(q) {
  return QUALITY_LABELS[q] ?? "";
}

/** "FLAC 96/24" — the chip under a now-playing title. */
export function formatChip(track) {
  if (!track) return "";
  const bits = [];
  if (track.codec) bits.push(track.codec);
  if (track.isDsd) return bits.join(" ") || "DSD";
  if (track.rateHz) {
    const khz = (track.rateHz / 1000).toFixed(track.rateHz % 1000 ? 1 : 0);
    bits.push(track.bits ? `${khz}/${track.bits}` : `${khz} kHz`);
  }
  return bits.join(" ");
}

/**
 * What this browser claims it can decode.
 *
 * Probed ONCE at boot and cached. canPlayType is a weak signal, not a
 * verdict — it answers "" for codec strings an engine doesn't
 * RECOGNISE even when it can decode the file, and ALAC/AIFF support
 * diverges sharply between Safari and Chromium. So this is used only to
 * pre-empt the obviously hopeless; the real authority is a decode
 * attempt and the MEDIA_ERR_SRC_NOT_SUPPORTED that follows.
 */
let engineSupport = null;

export function engineCanPlay(contentType) {
  if (!contentType) return false;
  if (!engineSupport) {
    const a = document.createElement("audio");
    engineSupport = (ct) => {
      const verdict = a.canPlayType(ct);
      return verdict === "probably" || verdict === "maybe";
    };
  }
  return engineSupport(contentType);
}

/**
 * Resolve what to actually request for a track, if anything.
 *
 * Returns { url, contentType, degraded } or null when nothing here can
 * play it. `degraded` marks a variant substitution so the UI can say
 * so — the web player is explicitly not a bit-exact path, and quietly
 * serving a different file than the one named would undercut that
 * honesty.
 */
export function resolvePlayable(track, audioURLFor) {
  const play = track && track.play;
  if (!play) return null;
  if (play.kind !== "none" && engineCanPlay(play.contentType)) {
    return { url: audioURLFor(track.path, null), contentType: play.contentType, degraded: false };
  }
  if (play.variantId && engineCanPlay(play.variantContentType)) {
    return {
      url: audioURLFor(track.path, play.variantId),
      contentType: play.variantContentType,
      degraded: true,
    };
  }
  // Engine-dependent with no variant: attempt it anyway. canPlayType
  // under-reports (notably ALAC), and a failed attempt costs one
  // request and surfaces a precise error, where refusing up front
  // would hide a track the browser could actually have played.
  if (play.kind === "engine-dependent") {
    return { url: audioURLFor(track.path, null), contentType: play.contentType, degraded: false };
  }
  return null;
}

/** Short reason for the "can't play here" chip. */
export function unplayableReason(track) {
  if (!track || !track.play) return "";
  if (track.play.kind === "none") {
    return track.isDsd ? "DSD — not playable in a browser" : "Not playable in a browser";
  }
  return "May not play in this browser";
}
