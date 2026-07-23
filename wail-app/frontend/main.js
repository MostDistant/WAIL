const { invoke } = window.__TAURI__.core;
const { listen } = window.__TAURI__.event;

// DOM elements
const firstLaunchScreen = document.getElementById('first-launch-screen');
const firstLaunchForm = document.getElementById('first-launch-form');
const firstLaunchNameInput = document.getElementById('first-launch-name');
const joinScreen = document.getElementById('join-screen');
const sessionScreen = document.getElementById('session-screen');
const joinForm = document.getElementById('join-form');
const joinBtn = document.getElementById('join-btn');
const joinError = document.getElementById('join-error');
const disconnectBtn = document.getElementById('disconnect-btn');
const sessionError = document.getElementById('session-error');
const sessionBpmInput = document.getElementById('session-bpm');
const linkClockEl = document.getElementById('link-clock');
const settingsBtn = document.getElementById('settings-btn');
const settingsPanel = document.getElementById('settings-panel');
const settingsCloseBtn = document.getElementById('settings-close-btn');
const settingsForm = document.getElementById('settings-form');
const settingsDisplayNameInput = document.getElementById('settings-display-name');
const settingsLinkAudioNameInput = document.getElementById('settings-link-audio-name');
const settingsTelemetryCheckbox = document.getElementById('settings-telemetry');
const settingsLogSharingCheckbox = document.getElementById('settings-log-sharing');
const settingsRememberCheckbox = document.getElementById('settings-remember');
const chatInput = document.getElementById('chat-input');
const chatSendBtn = document.getElementById('chat-send-btn');
const chatMessages = document.getElementById('chat-messages');

// Version labels (join screen header + Debug tab About)
window.__TAURI__.app.getVersion().then(v => {
  document.getElementById('version-label').textContent = 'v' + v;
  document.getElementById('debug-version').textContent = 'v' + v;
});

// Capture send-mixer: render discovered local Link Audio channels as checkboxes.
// Re-renders only when the channel set changes so ticking a box isn't clobbered
// by the 2s status refresh.
let _captureSig = '';
function renderCaptureMixer(channels) {
  const el = document.getElementById('capture-channels');
  if (!el) return;
  const sig = channels.map(c => `${c.channel_id}:${c.enabled}:${c.name}:${c.peer_name}`).join('|');
  if (sig === _captureSig) return;
  _captureSig = sig;
  if (channels.length === 0) {
    el.innerHTML = '<span class="empty">No local Link Audio channels discovered</span>';
    return;
  }
  el.innerHTML = '';
  channels.forEach(c => {
    const row = document.createElement('label');
    row.className = 'capture-channel';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = !!c.enabled;
    cb.addEventListener('change', () => {
      invoke('set_capture_enabled', { channelID: c.channel_id, enabled: cb.checked }).catch(() => {});
    });
    const name = document.createElement('span');
    name.textContent = c.peer_name ? `${c.peer_name} · ${c.name}` : (c.name || c.channel_id);
    row.appendChild(cb);
    row.appendChild(name);
    el.appendChild(row);
  });
}

// Debug capture-to-WAV dump toggle (live; engine defaults to off per session).
const captureDumpToggle = document.getElementById('capture-dump-toggle');
if (captureDumpToggle) {
  captureDumpToggle.addEventListener('change', () => {
    invoke('set_capture_dump', { enabled: captureDumpToggle.checked }).catch(() => {});
  });
}

// Debug server-echo loopback toggle (live; relay defaults to off per session).
const loopbackToggle = document.getElementById('loopback-toggle');
if (loopbackToggle) {
  loopbackToggle.addEventListener('change', () => {
    invoke('set_loopback', { enabled: loopbackToggle.checked }).catch(() => {});
  });
}

// WAIL Metronome toggle (live; engine defaults to off per session).
const metronomeToggle = document.getElementById('metronome-toggle');
if (metronomeToggle) {
  metronomeToggle.addEventListener('change', () => {
    invoke('set_metronome', { enabled: metronomeToggle.checked }).catch(() => {});
  });
}

// Emit-cushion slider (live). cushionUserSet gates the one-time initialisation
// from engine health so we don't clobber the user's drag with a stale snapshot.
const cushionSlider = document.getElementById('cushion-slider');
const cushionValue = document.getElementById('cushion-value');
let cushionUserSet = false;
if (cushionSlider) {
  cushionSlider.addEventListener('input', () => {
    cushionUserSet = true;
    if (cushionValue) cushionValue.textContent = cushionSlider.value + ' ms';
    invoke('set_cushion_ms', { ms: parseInt(cushionSlider.value, 10) }).catch(() => {});
  });
}


// --- Room Name Generator ---
// Dictionary 1: synthesis techniques, sound qualities, processing descriptors
const ROOM_MODIFIERS = [
  // Synthesis methods
  "Analog", "Digital", "Modular", "Granular", "Wavetable", "Spectral",
  "FM", "Additive", "Subtractive", "Physical", "Hybrid", "Generative",
  "Algorithmic", "Euclidean", "Stochastic", "Polyrhythmic", "Microtonal",
  "Bitcrushed", "Saturated", "Filtered",
  // Sound texture
  "Resonant", "Distorted", "Lush", "Bright", "Dark", "Warm", "Muted",
  "Punchy", "Dense", "Sparse", "Rich", "Thick", "Heavy", "Massive",
  "Delicate", "Hollow", "Crisp", "Tight", "Deep", "Raw",
  // Material texture
  "Fuzzy", "Gritty", "Silky", "Airy", "Glassy", "Crystalline", "Murky",
  "Grainy", "Polished", "Rough", "Smooth", "Sharp", "Jagged", "Fractured",
  "Porous", "Metallic", "Wooden", "Translucent", "Sheer", "Brittle",
  // Musical character
  "Harmonic", "Dissonant", "Chromatic", "Pentatonic", "Melodic", "Rhythmic",
  "Syncopated", "Percussive", "Polyphonic", "Monophonic", "Atmospheric",
  "Ambient", "Cinematic", "Minimal", "Chaotic", "Ethereal", "Dynamic",
  "Textural", "Orchestral", "Improvised",
  // FX and processing
  "Compressed", "Reverberant", "Delayed", "Modulated", "Phased", "Flanged",
  "Chorused", "Trembling", "Chopped", "Detuned", "Looped", "Sampled",
  "Processed", "Quantized", "Shuffled", "Swung", "Glitchy", "Warped",
  "Folded", "Stretched",
  // Motion and energy
  "Driving", "Pulsing", "Swirling", "Drifting", "Floating", "Swelling",
  "Soaring", "Rising", "Fading", "Cascading", "Spiraling", "Flowing",
  "Streaming", "Weaving", "Twisting", "Spinning", "Rolling", "Rumbling",
  "Surging", "Plunging",
  // Sonic texture
  "Crackling", "Humming", "Buzzing", "Droning", "Shimmering", "Ringing",
  "Echoing", "Chiming", "Hissing", "Growling", "Thundering", "Whispering",
  "Roaring", "Murmuring", "Sizzling", "Howling", "Tolling", "Clicking",
  "Snapping", "Sputtering",
  // Elemental and environmental
  "Liquid", "Frozen", "Glowing", "Burning", "Electric", "Acoustic",
  "Industrial", "Celestial", "Subterranean", "Volcanic", "Arctic", "Tropical",
  "Cosmic", "Galactic", "Lunar", "Solar", "Oceanic", "Glacial", "Abyssal",
  "Prismatic",
  // Vibe and mood
  "Serene", "Hypnotic", "Turbulent", "Aggressive", "Progressive",
  "Experimental", "Retro", "Vintage", "Futuristic", "Adaptive", "Radiant",
  "Shadowed", "Vibrant", "Fragile", "Robust", "Fluid", "Piercing", "Subtle",
  "Stark", "Tender",
  // Structural and abstract
  "Fractal", "Recursive", "Divergent", "Convergent", "Parallel", "Inverted",
  "Mirrored", "Nested", "Branching", "Tangled", "Cyclic", "Iterative",
  "Stacked", "Scattered", "Clustered", "Dispersed", "Morphing", "Evolving",
  "Unraveling", "Awakening",
];
// Dictionary 2: instruments, sound sources, and audio components
const ROOM_ELEMENTS = [
  // Synth voices and concepts
  "Synth", "Bass", "Arp", "Pad", "Lead", "Drone", "Pulse", "Chord",
  "Riff", "Patch", "Oscillator", "Filter", "Resonance", "LFO", "Envelope",
  "Sequencer", "Arpeggio", "Waveform", "Feedback", "Glitch",
  // String instruments
  "Guitar", "Piano", "Violin", "Cello", "Harp", "Lute", "Mandolin",
  "Banjo", "Sitar", "Koto", "Dulcimer", "Zither", "Ukulele", "Balalaika",
  "Bouzouki", "Oud", "Shamisen", "Erhu", "Guqin", "Theorbo",
  // Keys and organ family
  "Organ", "Harpsichord", "Clavichord", "Rhodes", "Wurlitzer", "Clavinet",
  "Mellotron", "Ondes", "Cristal", "Celesta",
  // Wind instruments
  "Flute", "Piccolo", "Oboe", "Clarinet", "Bassoon", "Saxophone", "Trumpet",
  "Trombone", "Tuba", "Flugelhorn", "Cornet", "Horn", "Recorder", "Ocarina",
  "Harmonica", "Accordion", "Bagpipes", "Didgeridoo", "Duduk", "Shakuhachi",
  // Tuned percussion
  "Marimba", "Vibraphone", "Xylophone", "Glockenspiel", "Theremin",
  "Kalimba", "Mbira", "Balafon", "Handpan", "Steeldrum",
  // Drums and untuned percussion
  "Kick", "Snare", "Hat", "Tom", "Clap", "Cymbal", "Cowbell", "Bongo",
  "Conga", "Tabla", "Djembe", "Cajon", "Maracas", "Tambourine", "Gong",
  "Woodblock", "Triangle", "Clave", "Rimshot", "Shaker",
  // FX units and processors
  "Reverb", "Delay", "Chorus", "Flanger", "Phaser", "Distortion",
  "Overdrive", "Bitcrusher", "Ringmod", "Waveshaper", "Limiter",
  "Compressor", "Clipper", "Saturator", "Expander", "Vocoder", "Sampler",
  "Transient", "Preamp", "Sidechain",
  // Modular and studio gear
  "VCO", "VCF", "VCA", "Mixer", "Attenuator", "Quantizer", "Module",
  "Rack", "Trigger", "Clock", "MIDI", "Keyboard", "Fader", "Crossfader",
  "Interface", "Controller", "Patchbay", "Bus", "Matrix", "Oscilloscope",
  // Vocal and choral
  "Voice", "Choir", "Vox", "Whisper", "Breath", "Scat", "Beatbox",
  "Chant", "Hymn", "Yodel", "Canon", "Fugue", "Round", "Ballad",
  "Lullaby", "Dirge", "Shanty", "Spiritual", "Mantra", "Call",
  // Sound and signal concepts
  "Noise", "Signal", "Frequency", "Amplitude", "Spectrum", "Overtone",
  "Undertone", "Resonator", "Transducer", "Exciter",
  // Music theory
  "Tone", "Note", "Pitch", "Timbre", "Scale", "Mode", "Harmony", "Melody",
  "Rhythm", "Cadence", "Phrase", "Motif", "Theme", "Ostinato", "Groove",
  "Beat", "Loop", "Sample", "Measure", "Tempo",
  // Acoustic and natural sound sources
  "Bell", "Chime", "Reed", "Bow", "Membrane", "Mallet", "Bowl", "Spring",
  "Wire", "Fork",
];
// Dictionary 3: venues, events, states, and places
const ROOM_VENUES = [
  // Music venues and events
  "Jam", "Session", "Lounge", "Studio", "Stage", "Gig", "Show",
  "Concert", "Festival", "Showcase", "Performance", "Exhibition", "Revue",
  "Recital", "Rehearsal", "Residency", "Soundcheck", "Opener", "Encore", "Set",
  // Abstract spaces and structures
  "Chamber", "Vault", "Nexus", "Temple", "Asylum", "Haven", "Forge",
  "Portal", "Labyrinth", "Sanctum", "Vortex", "Core", "Hub", "Crypt",
  "Tower", "Keep", "Spire", "Nave", "Atrium", "Rotunda",
  // Celestial and cosmic
  "Orbit", "Eclipse", "Nebula", "Cosmos", "Horizon", "Zenith", "Meridian",
  "Solstice", "Equinox", "Singularity", "Nova", "Pulsar", "Quasar",
  "Galaxy", "Aphelion", "Perihelion", "Transit", "Conjunction", "Nadir", "Apex",
  // Earthly geography and nature
  "Canyon", "Cavern", "Grotto", "Forest", "Meadow", "Tundra", "Delta",
  "Reef", "Glacier", "Volcano", "Mesa", "Basin", "Archipelago", "Fjord",
  "Estuary", "Bayou", "Savanna", "Plateau", "Atoll", "Peninsula",
  // Architectural
  "Cathedral", "Arena", "Amphitheater", "Citadel", "Fortress", "Monastery",
  "Abbey", "Basilica", "Pavilion", "Gallery", "Cloister", "Colonnade",
  "Arcade", "Stronghold", "Rampart", "Bastion", "Outpost", "Station",
  "Terminal", "Junction",
  // Mythological and fantastical
  "Realm", "Domain", "Lair", "Refuge", "Hideout", "Threshold", "Passage",
  "Gateway", "Crossroads", "Waypoint", "Dimension", "Plane", "Stratum",
  "Sector", "Territory", "Province", "Circuit", "Ring", "Spiral", "Field",
  // Water and flow
  "Ocean", "Sea", "Lake", "River", "Spring", "Harbor", "Cove", "Lagoon",
  "Gulf", "Strait", "Surge", "Tide", "Ebb", "Flux", "Current",
  "Stream", "Channel", "Pool", "Cascade", "Torrent",
  // Temporal states and phases
  "Dawn", "Dusk", "Twilight", "Midnight", "Genesis", "Origin", "Terminus",
  "Coda", "Finale", "Prologue", "Epilogue", "Interlude", "Overture",
  "Climax", "Resolution", "Summit", "Drift", "Epoch", "Aeon", "Vesper",
  // Atmosphere and light
  "Aurora", "Prism", "Haze", "Fog", "Mist", "Shadow", "Glow", "Gleam",
  "Shimmer", "Glare", "Blaze", "Flare", "Spark", "Ember", "Ash",
  "Smoke", "Storm", "Thunder", "Lightning", "Rainbow",
  // Institutions and places of learning
  "Legacy", "Vision", "Quest", "Odyssey", "Workshop", "Laboratory",
  "Academy", "Archive", "Conservatory", "Observatory", "Auditorium",
  "Colosseum", "Scriptorium", "Atelier", "Foundry", "Greenhouse",
  "Salon", "Parlor", "Ballroom", "Planetarium",
];
function generateRoomName() {
  const pick = arr => arr[Math.floor(Math.random() * arr.length)];
  return pick(ROOM_MODIFIERS) + pick(ROOM_ELEMENTS) + pick(ROOM_VENUES);
}
document.getElementById('generate-room-btn').addEventListener('click', () => {
  document.getElementById('room').value = generateRoomName();
});

// State
let unlisten = [];
let roomRefreshTimer = null;

// Rolling stats window state
const STATS_WINDOW_SIZE = 60; // 60 ticks x 2s = 2 minutes
let statsMode = 'all';        // 'all' or 'recent'
let statusSnapshots = [];
let lastStatusPayload = null;

// --- Display Name Storage ---
const DISPLAY_NAME_KEY = 'wail-display-name';
const LINK_AUDIO_NAME_KEY = 'wail-link-audio-name';
const DEFAULT_LINK_AUDIO_NAME = 'WAIL';
const TELEMETRY_KEY = 'wail-telemetry';
const LOG_SHARING_KEY = 'wail-log-sharing';
const REMEMBER_KEY = 'wail-remember';

function getDisplayName() {
  return localStorage.getItem(DISPLAY_NAME_KEY) || '';
}

function saveDisplayName(name) {
  localStorage.setItem(DISPLAY_NAME_KEY, name);
}

function getLinkAudioName() {
  return localStorage.getItem(LINK_AUDIO_NAME_KEY) || DEFAULT_LINK_AUDIO_NAME;
}

function saveLinkAudioName(name) {
  localStorage.setItem(LINK_AUDIO_NAME_KEY, name || DEFAULT_LINK_AUDIO_NAME);
}

function getTelemetryEnabled() {
  const val = localStorage.getItem(TELEMETRY_KEY);
  return val === null ? true : val === 'true';
}

function saveTelemetryEnabled(enabled) {
  localStorage.setItem(TELEMETRY_KEY, enabled ? 'true' : 'false');
}

function getLogSharingEnabled() {
  const val = localStorage.getItem(LOG_SHARING_KEY);
  return val === 'true';
}

function saveLogSharingEnabled(enabled) {
  localStorage.setItem(LOG_SHARING_KEY, enabled ? 'true' : 'false');
}

function getRememberEnabled() {
  const val = localStorage.getItem(REMEMBER_KEY);
  return val === null ? true : val === 'true';
}

function saveRememberEnabled(enabled) {
  localStorage.setItem(REMEMBER_KEY, enabled ? 'true' : 'false');
}

// --- Remember settings ---
const STORAGE_KEY = 'wail-settings';
const rememberFields = ['room', 'password', 'bpi', 'quantum', 'recording-enabled', 'recording-dir', 'recording-stems', 'recording-retention'];

function loadSettings() {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (!saved) return;
    const settings = JSON.parse(saved);
    for (const id of rememberFields) {
      if (settings[id] != null) {
        const el = document.getElementById(id);
        if (el.type === 'checkbox') {
          el.checked = settings[id];
        } else {
          el.value = settings[id];
        }
      }
    }
  } catch (_) {}
}

function saveSettings() {
  if (!getRememberEnabled()) {
    localStorage.removeItem(STORAGE_KEY);
    return;
  }
  const settings = {};
  for (const id of rememberFields) {
    const el = document.getElementById(id);
    settings[id] = el.type === 'checkbox' ? el.checked : el.value;
  }
  localStorage.setItem(STORAGE_KEY, JSON.stringify(settings));
}

function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

loadSettings();

// Restore recording options visibility after settings load
if (document.getElementById('recording-enabled').checked) {
  document.getElementById('recording-options').style.display = '';
}

// --- First Launch Detection ---
function showFirstLaunch() {
  firstLaunchScreen.style.display = 'flex';
  joinScreen.style.display = 'none';
  firstLaunchNameInput.focus();
}

function showJoinScreen() {
  firstLaunchScreen.style.display = 'none';
  joinScreen.style.display = '';
}

// On page load, check if display name is set
if (!getDisplayName()) {
  showFirstLaunch();
} else {
  showJoinScreen();
}

// First launch form submit
firstLaunchForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  const name = firstLaunchNameInput.value.trim();
  if (name) {
    saveDisplayName(name);
    showJoinScreen();
  }
});

// --- Join screen tab switching ---
const tabJoinBtn = document.getElementById('tab-join');
const tabPublicBtn = document.getElementById('tab-public');
const tabJoinContent = document.getElementById('tab-join-content');
const tabPublicContent = document.getElementById('tab-public-content');

tabJoinBtn.addEventListener('click', () => {
  tabJoinBtn.classList.add('active');
  tabPublicBtn.classList.remove('active');
  tabJoinContent.style.display = '';
  tabPublicContent.style.display = 'none';
  stopRoomRefresh();
});

tabPublicBtn.addEventListener('click', () => {
  tabPublicBtn.classList.add('active');
  tabJoinBtn.classList.remove('active');
  tabJoinContent.style.display = 'none';
  tabPublicContent.style.display = '';
  fetchPublicRooms();
  startRoomRefresh();
});

function startRoomRefresh() {
  stopRoomRefresh();
  roomRefreshTimer = setInterval(fetchPublicRooms, 10000);
}

function stopRoomRefresh() {
  if (roomRefreshTimer) {
    clearInterval(roomRefreshTimer);
    roomRefreshTimer = null;
  }
}

async function fetchPublicRooms() {
  try {
    const rooms = await invoke('list_public_rooms');
    renderPublicRooms(rooms);
  } catch (err) {
    document.getElementById('public-rooms-list').innerHTML =
      `<span class="empty">Failed to load: ${escapeHtml(String(err))}</span>`;
  }
}

function renderPublicRooms(rooms) {
  const list = document.getElementById('public-rooms-list');
  if (rooms.length === 0) {
    list.innerHTML = '<span class="empty">No public rooms available</span>';
    return;
  }
  list.innerHTML = rooms.map(r => {
    const bpm = r.bpm ? `${r.bpm.toFixed(0)} BPM` : '-- BPM';
    const names = r.display_names.filter(Boolean).join(', ') || 'anonymous';
    return `<div class="room-card">
      <div class="room-info">
        <span class="room-name">${escapeHtml(r.room)}</span>
        <span class="room-meta">${r.peer_count} peer${r.peer_count !== 1 ? 's' : ''} &middot; ${bpm} &middot; ${escapeHtml(names)}</span>
      </div>
      <button type="button" data-room="${escapeHtml(r.room)}">Join</button>
    </div>`;
  }).join('');

  // Attach click handlers
  list.querySelectorAll('.room-card button').forEach(btn => {
    btn.addEventListener('click', () => joinPublicRoom(btn.dataset.room));
  });
}

async function joinPublicRoom(room) {
  const m = bpiMath();
  if (!m || !m.whole) {
    showError(joinError, 'Beats per interval must divide evenly into whole bars');
    return;
  }
  const params = {
    room: room,
    password: null,
    displayName: getDisplayName(),
    linkAudioName: getLinkAudioName(),
    bpm: 120.0,
    bars: m.bars,
    quantum: m.bpb,
    recordingEnabled: document.getElementById('recording-enabled').checked,
    recordingDirectory: document.getElementById('recording-dir').value || null,
    recordingStems: document.getElementById('recording-stems').checked,
    recordingRetentionDays: parseInt(document.getElementById('recording-retention').value) || 30,
  };
  try {
    const result = await invoke('join_room', params);
    saveSettings();
    stopRoomRefresh();
    showSession(result.room);
    setupListeners();
  } catch (err) {
    showError(joinError, err);
  }
}

document.getElementById('refresh-rooms-btn').addEventListener('click', fetchPublicRooms);

// --- Beats per interval (ADR-0004) ---
// BPI is the user-facing interval length; the wire model stays bars x beats
// per bar, so BPI must divide evenly into whole bars.
const bpiInput = document.getElementById('bpi');
const quantumInput = document.getElementById('quantum');

function bpiMath() {
  const bpi = parseFloat(bpiInput.value);
  const bpb = parseFloat(quantumInput.value);
  if (!(bpi > 0) || !(bpb > 0)) return null;
  const bars = bpi / bpb;
  return { bpi, bpb, bars, whole: Number.isInteger(bars) };
}

function updateBpiHelper() {
  const el = document.getElementById('quant-summary');
  const m = bpiMath();
  if (!m) { el.textContent = ''; el.classList.remove('error-text'); return; }
  if (m.whole) {
    const barsWord = `${m.bars} Bar${m.bars !== 1 ? 's' : ''}`;
    el.innerHTML =
      `<span class="quant-summary-eq">BEATS PER INTERVAL ${m.bpi} / BEATS PER BAR ${m.bpb} =</span>` +
      `<span class="quant-summary-result">Set your Global Launch Quantization to ${barsWord}</span>`;
    el.classList.remove('error-text');
  } else {
    el.textContent = `${m.bars.toFixed(2)} bars in ${m.bpb}/4 — must be a whole number of bars`;
    el.classList.add('error-text');
  }
}

bpiInput.addEventListener('input', updateBpiHelper);
quantumInput.addEventListener('input', updateBpiHelper);
updateBpiHelper();

// --- Recording options toggle ---
document.getElementById('recording-enabled').addEventListener('change', (e) => {
  document.getElementById('recording-options').style.display = e.target.checked ? '' : 'none';
});

document.getElementById('browse-recording-dir').addEventListener('click', async () => {
  try {
    const dir = await invoke('get_default_recording_dir');
    document.getElementById('recording-dir').value = dir;
  } catch (err) {
    console.error('Failed to get default recording dir:', err);
  }
});

// Sync telemetry, log sharing, and remember-settings state on load
invoke('set_telemetry', { enabled: getTelemetryEnabled() }).catch(() => {});
invoke('set_log_sharing', { enabled: getLogSharingEnabled() }).catch(() => {});
invoke('set_remember_enabled', { enabled: getRememberEnabled() }).catch(() => {});

// Populate default recording dir on load
invoke('get_default_recording_dir').then(dir => {
  const el = document.getElementById('recording-dir');
  if (!el.value) el.value = dir;
}).catch(() => {});

// --- Settings Panel ---
function openSettings() {
  settingsDisplayNameInput.value = getDisplayName();
  settingsLinkAudioNameInput.value = getLinkAudioName();
  settingsTelemetryCheckbox.checked = getTelemetryEnabled();
  settingsLogSharingCheckbox.checked = getLogSharingEnabled();
  settingsRememberCheckbox.checked = getRememberEnabled();
  settingsPanel.style.display = 'flex';
}

settingsBtn.addEventListener('click', openSettings);
document.getElementById('session-settings-btn').addEventListener('click', openSettings);

document.getElementById('interval-prompt-ok').addEventListener('click', () => {
  document.getElementById('interval-prompt').style.display = 'none';
});
document.getElementById('interval-prompt-close').addEventListener('click', () => {
  document.getElementById('interval-prompt').style.display = 'none';
});

settingsCloseBtn.addEventListener('click', () => {
  settingsPanel.style.display = 'none';
});

settingsPanel.addEventListener('click', (e) => {
  if (e.target === settingsPanel) {
    settingsPanel.style.display = 'none';
  }
});

settingsForm.addEventListener('submit', (e) => {
  e.preventDefault();
  const name = settingsDisplayNameInput.value.trim();
  if (name) {
    saveDisplayName(name);
  }
  // Link Audio name: blank falls back to the default.
  saveLinkAudioName(settingsLinkAudioNameInput.value.trim());
  // Save telemetry setting
  const telemetryEnabled = settingsTelemetryCheckbox.checked;
  saveTelemetryEnabled(telemetryEnabled);
  invoke('set_telemetry', { enabled: telemetryEnabled }).catch(() => {});
  // Save log sharing setting
  const logSharingEnabled = settingsLogSharingCheckbox.checked;
  saveLogSharingEnabled(logSharingEnabled);
  invoke('set_log_sharing', { enabled: logSharingEnabled }).catch(() => {});
  // Save remember setting
  const rememberEnabled = settingsRememberCheckbox.checked;
  saveRememberEnabled(rememberEnabled);
  invoke('set_remember_enabled', { enabled: rememberEnabled }).catch(() => {});
  if (rememberEnabled) {
    saveSettings();
  } else {
    localStorage.removeItem(STORAGE_KEY);
  }
  settingsPanel.style.display = 'none';
});

// --- Session screen tab switching ---
const sessionTabSessionBtn = document.getElementById('session-tab-session');
const sessionTabChatBtn = document.getElementById('session-tab-chat');
const sessionTabDebugBtn = document.getElementById('session-tab-debug');
const sessionTabSessionContent = document.getElementById('session-tab-session-content');
const sessionTabChatContent = document.getElementById('session-tab-chat-content');
const sessionTabDebugContent = document.getElementById('session-tab-debug-content');

const SESSION_TABS = [
  { btn: sessionTabSessionBtn, content: sessionTabSessionContent },
  { btn: sessionTabChatBtn,    content: sessionTabChatContent },
  { btn: sessionTabDebugBtn,   content: sessionTabDebugContent },
];

function switchSessionTab(activeBtn) {
  SESSION_TABS.forEach(({ btn, content }) => {
    btn.classList.toggle('active', btn === activeBtn);
    content.style.display = btn === activeBtn ? '' : 'none';
  });
}

sessionTabSessionBtn.addEventListener('click', () => switchSessionTab(sessionTabSessionBtn));
sessionTabChatBtn.addEventListener('click', () => switchSessionTab(sessionTabChatBtn));
sessionTabDebugBtn.addEventListener('click', () => switchSessionTab(sessionTabDebugBtn));

function resetStatsWindow() {
  statusSnapshots = [];
  statsMode = 'all';
  lastStatusPayload = null;
  document.getElementById('stats-mode-btn').textContent = 'all time';
}

function showJoin() {
  firstLaunchScreen.style.display = 'none';
  joinScreen.style.display = '';
  sessionScreen.style.display = 'none';
  joinError.style.display = 'none';
  joinBtn.disabled = false;
  joinBtn.textContent = 'Join Room';
  switchSessionTab(sessionTabSessionBtn);
  resetStatsWindow();
  hideClock();
  cleanup();
}

function showSession(room) {
  joinScreen.style.display = 'none';
  sessionScreen.style.display = '';
  sessionError.style.display = 'none';
  if (captureDumpToggle) captureDumpToggle.checked = false; // new session → dump off
  if (loopbackToggle) loopbackToggle.checked = false; // new session → loopback off
  if (metronomeToggle) metronomeToggle.checked = false; // new session → metronome off
  cushionUserSet = false; // re-sync the cushion slider from the new engine
  resetStatsWindow();
  clearLog();
  clearChatMessages();
  document.getElementById('session-room').textContent = room;
  document.getElementById('peer-tree').innerHTML = '<span class="empty">No peers connected</span>';
  document.getElementById('peer-detail').innerHTML = '<span class="empty">No peers connected</span>';
  const peerCount = document.getElementById('peer-count');
  peerCount.textContent = '0/0';
  peerCount.className = 'log-badge';
  document.getElementById('session-audio').textContent = '0 / 0';
  document.getElementById('session-audio-bytes').textContent = '0 B / 0 B';
  document.getElementById('session-link-peers').textContent = '0';
  hideClock(); // stays hidden until the first status:update seeds it
  document.getElementById('session-bpi').value = '';
  document.getElementById('session-interval-bars').textContent = '-';
  document.getElementById('recording-stat').style.display =
    document.getElementById('recording-enabled').checked ? '' : 'none';
}

function showError(el, msg) {
  el.textContent = msg;
  el.style.display = '';
}

function cleanup() {
  unlisten.forEach(fn => fn());
  unlisten = [];
}

// --- Join ---
joinForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  joinError.style.display = 'none';
  joinBtn.disabled = true;
  joinBtn.textContent = 'Connecting...';

  const m = bpiMath();
  if (!m || !m.whole) {
    showError(joinError, 'Beats per interval must divide evenly into whole bars');
    joinBtn.disabled = false;
    joinBtn.textContent = 'Join Room';
    return;
  }
  const params = {
    room: document.getElementById('room').value,
    password: document.getElementById('password').value || null,
    displayName: getDisplayName(),
    linkAudioName: getLinkAudioName(),
    bpm: 120.0,
    bars: m.bars,
    quantum: m.bpb,
    recordingEnabled: document.getElementById('recording-enabled').checked,
    recordingDirectory: document.getElementById('recording-dir').value || null,
    recordingStems: document.getElementById('recording-stems').checked,
    recordingRetentionDays: parseInt(document.getElementById('recording-retention').value) || 30,
  };

  try {
    const result = await invoke('join_room', params);
    saveSettings();
    showSession(result.room);
    setupListeners();
  } catch (err) {
    showError(joinError, err);
    joinBtn.disabled = false;
    joinBtn.textContent = 'Join Room';
  }
});

// --- Disconnect ---
disconnectBtn.addEventListener('click', async () => {
  try {
    await invoke('disconnect');
  } catch (err) {
    console.error('Disconnect error:', err);
  }
  showJoin();
});

// --- Set BPM (on Enter or blur) ---
async function applyBpm() {
  const bpm = parseFloat(sessionBpmInput.value);
  if (isNaN(bpm) || bpm < 20 || bpm > 999) return;
  try {
    await invoke('change_bpm', { bpm });
  } catch (err) {
    console.error('BPM change error:', err);
  }
}

sessionBpmInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') {
    e.preventDefault();
    sessionBpmInput.blur();
  }
});

sessionBpmInput.addEventListener('change', applyBpm);

// --- Set interval BPI (ADR-0004: anyone may change it; applies at the next
// interval boundary, relay reanchors the room clock) ---
const sessionBpiInput = document.getElementById('session-bpi');
let roomQuantum = 4;
let lastIntervalBars = 4;

async function applyBpi() {
  const bpi = parseInt(sessionBpiInput.value);
  const bars = bpi / roomQuantum;
  if (!(bpi > 0) || !Number.isInteger(bars)) {
    showError(sessionError, `Beats per interval must be a whole number of bars at ${roomQuantum} beats per bar`);
    sessionBpiInput.value = Math.round(lastIntervalBars * roomQuantum);
    return;
  }
  if (bars === lastIntervalBars) return;
  sessionError.style.display = 'none';
  try {
    await invoke('set_interval', { bars, quantum: roomQuantum });
  } catch (err) {
    showError(sessionError, err);
  }
}

sessionBpiInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') {
    e.preventDefault();
    sessionBpiInput.blur();
  }
});
sessionBpiInput.addEventListener('change', applyBpi);

// --- Stats mode toggle click handlers ---
document.getElementById('stats-mode-btn').addEventListener('click', toggleStatsMode);

// --- Chat ---
chatSendBtn.addEventListener('click', sendChatMessage);
chatInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') {
    e.preventDefault();
    sendChatMessage();
  }
});

// --- Stats mode toggle ---
function toggleStatsMode() {
  statsMode = statsMode === 'all' ? 'recent' : 'all';
  const label = statsMode === 'all' ? 'all time' : 'last 2 min';
  document.getElementById('stats-mode-btn').textContent = label;
  if (lastStatusPayload) renderStatus(lastStatusPayload);
}

function renderStatus(s) {
  const bpmInput = sessionBpmInput;
  if (document.activeElement !== bpmInput) {
    bpmInput.value = s.bpm.toFixed(1);
  }
  document.getElementById('session-link-peers').textContent = s.link_peers;
  document.getElementById('link-no-peers-warning').style.display =
    (s.link_peers === 0 && s.plugin_connected) ? '' : 'none';

  // Compute display values (windowed or cumulative)
  let sent = s.audio_sent, recv = s.audio_recv;
  let bytesSent = s.audio_bytes_sent, bytesRecv = s.audio_bytes_recv;
  if (statsMode === 'recent' && statusSnapshots.length > 1) {
    const oldest = statusSnapshots[0];
    sent = Math.max(0, s.audio_sent - oldest.audio_sent);
    recv = Math.max(0, s.audio_recv - oldest.audio_recv);
    bytesSent = Math.max(0, s.audio_bytes_sent - oldest.audio_bytes_sent);
    bytesRecv = Math.max(0, s.audio_bytes_recv - oldest.audio_bytes_recv);
  }
  document.getElementById('session-audio').textContent = `${sent} / ${recv}`;
  document.getElementById('session-audio-bytes').textContent =
    `${formatBytes(bytesSent)} / ${formatBytes(bytesRecv)}`;

  // Interval display: beats-first, bars in parens (ADR-0004).
  roomQuantum = s.interval_quantum || 4;
  lastIntervalBars = s.interval_bars;
  const roomBpi = Math.round(s.interval_bars * roomQuantum);
  if (document.activeElement !== sessionBpiInput) {
    sessionBpiInput.value = roomBpi;
  }
  document.getElementById('session-interval-bars').textContent =
    `(${s.interval_bars} bar${s.interval_bars !== 1 ? 's' : ''})`;

  // Capture send-mixer: discovered local Link Audio channels (own republished
  // channels never reach this list — the engine excludes them).
  renderCaptureMixer(s.capture_channels || []);

  // Update recording status
  if (s.recording) {
    document.getElementById('recording-stat').style.display = '';
    const mb = (s.recording_size_bytes / (1024 * 1024)).toFixed(1);
    document.getElementById('recording-size').textContent = `${mb} MB`;
  }

  renderPeerTree(s);
  renderPeerDetail(s);
}

// --- Interval clock (Ableton Link) ---
// A countdown pie in the header: full at each interval boundary, emptying
// clockwise from noon over one interval (BPI beats). status:update only fires
// every 2s, so we extrapolate the beat with requestAnimationFrame between events.
let clockSync = null; // { beat0, bpm, bpi, t0 } re-seeded from each status:update
let clockRAF = 0;

function seedClock(s) {
  // BPI = interval_bars × interval_quantum (same derivation as roomBpi above).
  const bpi = (s.interval_bars || 0) * (s.interval_quantum || 0);
  clockSync = { beat0: s.beat, bpm: s.bpm, bpi, t0: performance.now() };
  linkClockEl.style.display = '';
  startClock();
}

function startClock() {
  if (!clockRAF) clockRAF = requestAnimationFrame(tickClock);
}

function stopClock() {
  if (clockRAF) cancelAnimationFrame(clockRAF);
  clockRAF = 0;
}

function hideClock() {
  stopClock();
  clockSync = null;
  linkClockEl.style.display = 'none';
  linkClockEl.style.background = '';
}

function tickClock() {
  clockRAF = 0;
  if (!clockSync || clockSync.bpi <= 0 || clockSync.bpm <= 0) {
    linkClockEl.style.background = 'var(--clock-empty)';
    return; // nothing to pace; the next status:update will re-seed and restart us
  }
  const beat = clockSync.beat0 +
    ((performance.now() - clockSync.t0) / 1000) * (clockSync.bpm / 60);
  const into = (((beat % clockSync.bpi) + clockSync.bpi) % clockSync.bpi);
  const deg = (into / clockSync.bpi) * 360;
  linkClockEl.style.background =
    `conic-gradient(var(--clock-empty) 0 ${deg}deg, var(--clock-fill) ${deg}deg 360deg)`;
  clockRAF = requestAnimationFrame(tickClock);
}

// Pause the sweep while the window is hidden; on return, re-seed the time origin
// from the last status so it doesn't jump.
document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    stopClock();
  } else if (clockSync) {
    if (lastStatusPayload) seedClock(lastStatusPayload);
    else startClock();
  }
});

// Engine health counters: [json key, friendly label]. Each increment is a
// likely-audible event on the local audio path.
const HEALTH_FIELDS = [
  ['capture_ring_dropped', 'Capture ring drops'],
  ['capture_lan_lost_buffers', 'Capture LAN loss (buffers)'],
  ['capture_lan_gap_events', 'Capture LAN loss (events)'],
  ['capture_resnaps', 'Capture re-anchors (drift snaps)'],
  ['capture_slews', 'Drift micro-slews (inaudible)'],
  ['capture_dropped_late', 'Capture late drops'],
  ['capture_dropped_backfill', 'Capture backfill drops'],
  ['emit_sink_underrun_events', 'Sink underruns (audible dropouts)'],
  ['emit_sink_underrun_frames', 'Sink underrun frames'],
  ['emit_frames_missing_at_play', 'Frames missing at playout'],
  ['emit_frames_concealed', 'Frames concealed (Opus PLC)'],
  ['emit_intervals_incomplete', 'Released before tail (benign)'],
  ['wire_decode_failures', 'Wire decode failures'],
  ['opus_decode_failures', 'Opus decode failures'],
];

// Non-zero values that are expected in normal operation — not styled as errors.
const HEALTH_BENIGN = new Set(['emit_intervals_incomplete', 'emit_frames_concealed', 'capture_slews']);

function renderHealth(health) {
  const el = document.getElementById('network-health');
  if (!el || !health) return;
  el.innerHTML = HEALTH_FIELDS.map(([key, label]) => {
    const v = health[key] || 0;
    const cls = v > 0 && !HEALTH_BENIGN.has(key) ? 'health-bad' : '';
    return `<div class="health-row"><span>${label}</span><span class="${cls}">${v}</span></div>`;
  }).join('');
}

// Unified peers tree (Session tab): a "you" node carrying your sends —
// enabled capture channels and in-app senders (test tone / WAV), names
// click-to-rename — above the remote peer nodes with their channels nested
// beneath. Built from the status:update payload: local_sends carries your
// sends with effective names, peers carry the roster, slots carry each
// remote peer's channels. A peer counts as "sending" when we're presently
// receiving its audio (is_receiving). A status tick never clobbers a
// rename-in-progress.
function renderPeerTree(s) {
  const treeEl = document.getElementById('peer-tree');
  const badge = document.getElementById('peer-count');
  const peers = s.peers || [];
  const slots = s.slots || [];

  if (treeEl.querySelector('.stream-name-input')) return; // rename in progress

  // Group channels under their peer. peer_id ties a slot to a roster entry;
  // slots whose peer has dropped (id empty / not in the roster) are shown as
  // their own reconnecting node so no channel silently disappears.
  const channelsByPeer = new Map();
  for (const sl of slots) {
    const key = sl.peer_id || `client:${sl.client_id}`;
    if (!channelsByPeer.has(key)) channelsByPeer.set(key, []);
    channelsByPeer.get(key).push(sl);
  }

  const nodes = peers.map(p => ({
    name: p.display_name || p.peer_id.slice(0, 8),
    status: p.status || 'connecting',
    sending: !!p.is_receiving,
    channels: channelsByPeer.get(p.peer_id) || [],
  }));
  const roster = new Set(peers.map(p => p.peer_id));
  for (const [key, chans] of channelsByPeer) {
    if (roster.has(key)) continue;
    nodes.push({
      name: chans[0].display_name || chans[0].short_id || key,
      status: 'reconnecting',
      sending: chans.some(c => c.is_receiving),
      channels: chans,
    });
  }
  nodes.sort((a, b) => a.name.localeCompare(b.name));

  // Count reflects the roster: peers presently sending / total connected.
  const sending = peers.filter(p => p.is_receiving).length;
  badge.textContent = `${sending}/${peers.length}`;
  badge.className = 'log-badge' + (sending > 0 ? ' has-activity' : '');

  const youHtml = renderYouNode(s.local_sends || []);
  if (nodes.length === 0 && !youHtml) {
    treeEl.innerHTML = '<span class="empty">No peers connected</span>';
    return;
  }
  treeEl.innerHTML = youHtml + nodes.map(renderPeerNode).join('');
  treeEl.querySelectorAll('.peer-name.editable').forEach(span => {
    span.addEventListener('click', startStreamNameEdit);
  });
}

// The "you" node: one row per active send (enabled capture channel or in-app
// sender). Empty when you're sending nothing — the node then disappears and
// only remote peers show.
function renderYouNode(localSends) {
  if (localSends.length === 0) return '';
  const anySending = localSends.some(ls => ls.is_sending);
  const rows = localSends
    .slice()
    .sort((a, b) => a.stream_index - b.stream_index)
    .map(ls => {
      const label = ls.stream_name
        ? escapeHtml(ls.stream_name)
        : (localSends.length > 1 ? `My Send (stream ${ls.stream_index})` : 'My Send');
      return `<div class="peer-channel"><span class="channel-name peer-name editable" data-stream-index="${ls.stream_index}">${label}</span></div>`;
    }).join('');
  const statusClass = anySending ? 'status-receiving' : 'status-connected';
  const tag = anySending ? 'sending' : 'idle';
  return `<div class="peer-node peer-node--you">
    <div class="peer-node-head">
      <span class="peer-dot ${statusClass}"></span>
      <span class="peer-name">You</span>
      <span class="peer-status ${statusClass}">${tag}</span>
    </div>
    <div class="peer-channels">${rows}</div>
  </div>`;
}

function renderPeerNode(n) {
  const statusClass = n.sending ? 'status-receiving' : `status-${n.status}`;
  const tag = n.sending ? 'sending' : (n.status === 'connected' ? 'idle' : n.status);
  const chans = n.channels.slice().sort((a, b) => a.channel_index - b.channel_index);
  const channelsHtml = chans.length
    ? chans.map(c => {
        const label = c.stream_name ? escapeHtml(c.stream_name) : `stream ${c.channel_index}`;
        return `<div class="peer-channel"><span class="channel-name">${label}</span></div>`;
      }).join('')
    : '<div class="peer-channel peer-channel--none"><span class="channel-name">no channels</span></div>';
  return `<div class="peer-node">
    <div class="peer-node-head">
      <span class="peer-dot ${statusClass}"></span>
      <span class="peer-name">${escapeHtml(n.name)}</span>
      <span class="peer-status ${statusClass}">${escapeHtml(tag)}</span>
    </div>
    <div class="peer-channels">${channelsHtml}</div>
  </div>`;
}

// Debug tab: per-peer relay round-trip times (drives clock sync; not shown in
// the common path — it changes nothing a musician would do mid-jam).
function renderPeerDetail(s) {
  const el = document.getElementById('peer-detail');
  const peers = s.peers || [];
  if (peers.length === 0) {
    el.innerHTML = '<span class="empty">No peers connected</span>';
    return;
  }
  el.innerHTML = peers.map(p => {
    const name = p.display_name || p.peer_id.slice(0, 8);
    const rtt = p.rtt_ms != null ? `${p.rtt_ms.toFixed(0)}ms` : '—';
    const state = p.is_receiving ? 'sending' : (p.status || 'connecting');
    return `<div class="health-row"><span>${escapeHtml(name)}</span><span>${rtt} · ${escapeHtml(state)}</span></div>`;
  }).join('');
}

// --- Event Listeners ---
async function setupListeners() {
  cleanup();

  unlisten.push(await listen('status:update', (event) => {
    const s = event.payload;
    lastStatusPayload = s;
    statusSnapshots.push({
      audio_sent: s.audio_sent, audio_recv: s.audio_recv,
      audio_bytes_sent: s.audio_bytes_sent, audio_bytes_recv: s.audio_bytes_recv,
    });
    if (statusSnapshots.length > STATS_WINDOW_SIZE) statusSnapshots.shift();
    renderStatus(s);
    seedClock(s);
  }));

  unlisten.push(await listen('tempo:changed', (event) => {
    sessionBpmInput.value = event.payload.bpm.toFixed(1);
  }));

  unlisten.push(await listen('session:error', (event) => {
    showError(sessionError, event.payload.message);
  }));

  unlisten.push(await listen('interval:prompt', (event) => {
    const p = event.payload;
    const bpi = Math.round(p.bars * p.quantum);
    const barsWord = `${p.bars} bar${p.bars !== 1 ? 's' : ''}`;
    document.getElementById('interval-prompt-text').textContent =
      `This room runs ${bpi} beats (${barsWord}) per interval — set your DAW's launch quantization to ${barsWord} to match, then start your transport.`;
    document.getElementById('interval-prompt').style.display = '';
  }));

  unlisten.push(await listen('session:ended', () => {
    showJoin();
  }));

  unlisten.push(await listen('log:entry', (event) => {
    const p = event.payload;
    addLogEntry(p.level, p.message, p.peer_name || p.peer_id || null);
  }));

  unlisten.push(await listen('chat:message', (event) => {
    const p = event.payload;
    addChatMessage(p.sender_name, p.is_own, p.text);
  }));

  // The peer tree is driven by status:update (it carries peers + slots); this
  // event now only feeds the local audio-health counters beneath it.
  unlisten.push(await listen('peers:network', (event) => {
    renderHealth(event.payload.health);
    // Initialise the cushion slider from the engine's effective value once (it
    // reflects any WAIL_EMIT_CUSHION_MS override); the user's drag takes over after.
    const ms = event.payload.health && event.payload.health.emit_cushion_ms;
    if (cushionSlider && !cushionUserSet && ms) {
      cushionSlider.value = ms;
      if (cushionValue) cushionValue.textContent = ms + ' ms';
    }
  }));
}

// --- Log panel ---
let logEntries = [];
const MAX_LOG_ENTRIES = 200;

function addLogEntry(level, message, peerLabel) {
  const time = new Date().toLocaleTimeString();
  logEntries.push({ time, level, message });
  if (logEntries.length > MAX_LOG_ENTRIES) {
    logEntries.shift();
  }

  const logList = document.getElementById('log-list');
  const entry = document.createElement('div');
  entry.className = `log-entry ${level}${peerLabel ? ' peer-log' : ''}`;
  const peerPrefix = peerLabel ? `<span class="log-peer">[${escapeHtml(peerLabel)}]</span> ` : '';
  entry.innerHTML = `<span class="log-time">${time}</span>${peerPrefix}${escapeHtml(message)}`;
  logList.appendChild(entry);
  logList.scrollTop = logList.scrollHeight;

  // Trim DOM to match cap
  while (logList.children.length > MAX_LOG_ENTRIES) {
    logList.removeChild(logList.firstChild);
  }

  // Update badge
  const badge = document.getElementById('log-count');
  badge.textContent = logEntries.length;
  const hasErrors = logEntries.some(e => e.level === 'error');
  const hasWarnings = logEntries.some(e => e.level === 'warn');
  badge.className = 'log-badge' +
    (hasErrors ? ' has-errors' : hasWarnings ? ' has-warnings' : '');
}

function clearLog() {
  logEntries = [];
  document.getElementById('log-list').innerHTML = '';
  const badge = document.getElementById('log-count');
  badge.textContent = '0';
  badge.className = 'log-badge';
}

// --- Chat panel ---
const MAX_CHAT_ENTRIES = 200;

function sendChatMessage() {
  const text = chatInput.value.trim();
  if (!text) return;
  chatInput.value = '';
  invoke('send_chat', { text }).catch(err => console.error('Send chat error:', err));
}

function addChatMessage(senderName, isOwn, text) {
  const time = new Date().toLocaleTimeString();
  const entry = document.createElement('div');
  entry.className = 'chat-entry' + (isOwn ? ' chat-own' : '');

  const sender = document.createElement('span');
  sender.className = 'chat-sender';
  sender.textContent = isOwn ? 'You' : senderName;

  const timeSpan = document.createElement('span');
  timeSpan.className = 'chat-time';
  timeSpan.textContent = time;

  const messageText = document.createElement('span');
  messageText.className = 'chat-text';
  messageText.textContent = text;

  entry.appendChild(sender);
  entry.appendChild(timeSpan);
  entry.appendChild(messageText);

  chatMessages.appendChild(entry);

  // Cap at MAX_CHAT_ENTRIES
  while (chatMessages.children.length > MAX_CHAT_ENTRIES) {
    chatMessages.removeChild(chatMessages.firstChild);
  }

  // Auto-scroll to bottom
  chatMessages.scrollTop = chatMessages.scrollHeight;
}

function clearChatMessages() {
  chatMessages.innerHTML = '';
  chatInput.value = '';
}

function escapeHtml(text) {
  const div = document.createElement('div');
  div.textContent = text;
  return div.innerHTML;
}

function startStreamNameEdit(e) {
  const span = e.currentTarget;
  const streamIndex = parseInt(span.dataset.streamIndex, 10);
  const currentName = span.textContent;
  const input = document.createElement('input');
  input.type = 'text';
  input.className = 'stream-name-input';
  input.value = currentName.startsWith('My Send') ? '' : currentName;
  input.maxLength = 32;
  input.placeholder = 'Name this send...';

  let committed = false;
  let cancelled = false;
  const commit = () => {
    if (committed || cancelled) return;
    committed = true;
    const name = input.value.trim();
    invoke('rename_stream', { streamIndex, name });
    // The next status:update will re-render with the new name
  };

  input.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') {
      ev.preventDefault();
      commit();
      input.blur();
    } else if (ev.key === 'Escape') {
      ev.preventDefault();
      cancelled = true;
      input.replaceWith(span);
    }
  });
  input.addEventListener('blur', () => {
    if (!cancelled) commit();
  });

  span.replaceWith(input);
  input.focus();
  input.select();
}


// Check if a session was auto-started (e.g. via --test-room CLI flag)
invoke('get_active_session').then(result => {
  if (result) {
    showSession(result.room);
    setupListeners();
  }
}).catch(() => {});
