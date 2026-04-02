// Wails v3 adapter: provides a Tauri-compatible API surface so main.js works unchanged.
// This must be loaded BEFORE main.js.
//
// Tauri API:
//   window.__TAURI__.core.invoke("command_name", { arg1: val1 })
//   window.__TAURI__.event.listen("event:name", callback)
//   window.__TAURI__.app.getVersion()
//
// Wails v3 API:
//   wails.Call.ByName("main.App.MethodName", arg1, arg2, ...)
//   wails.Events.On("event:name", callback)

(function() {
    'use strict';

    // Map Tauri snake_case command names to Wails PascalCase method names on App.
    const commandMap = {
        'list_public_rooms': 'main.App.ListPublicRooms',
        'join_room': 'main.App.JoinRoom',
        'disconnect': 'main.App.Disconnect',
        'change_bpm': 'main.App.ChangeBPM',
        'send_chat': 'main.App.SendChat',
        'set_test_tone': 'main.App.SetTestTone',
        'set_telemetry': 'main.App.SetTelemetry',
        'set_log_sharing': 'main.App.SetLogSharing',
        'get_default_recording_dir': 'main.App.GetDefaultRecordingDir',
        'cleanup_recordings': 'main.App.CleanupRecordings',
        'get_active_session': 'main.App.GetActiveSession',
        'get_plugin_install_errors': 'main.App.GetPluginInstallErrors',
        'rename_stream': 'main.App.RenameStream',
    };

    // Tauri invoke passes a single object of named args.
    // Wails Call.ByName takes positional args matching the Go method signature.
    // This mapping converts named args to positional for each command.
    const argOrder = {
        'join_room': ['room', 'password', 'display_name', 'bpm', 'bars', 'quantum',
                       'recording_enabled', 'recording_directory', 'recording_stems',
                       'recording_retention_days', 'stream_count', 'test_mode'],
        'change_bpm': ['bpm'],
        'send_chat': ['text'],
        'set_test_tone': ['stream_index'],
        'set_telemetry': ['enabled'],
        'set_log_sharing': ['enabled'],
        'cleanup_recordings': ['directory', 'retention_days'],
        'rename_stream': ['stream_index', 'name'],
    };

    async function invoke(command, args) {
        const wailsMethod = commandMap[command];
        if (!wailsMethod) {
            console.warn('[wails-adapter] Unknown command:', command);
            throw new Error('Unknown command: ' + command);
        }

        // Convert named args to positional
        const order = argOrder[command];
        let positionalArgs;
        if (order && args) {
            positionalArgs = order.map(key => args[key] !== undefined ? args[key] : null);
        } else if (args) {
            positionalArgs = Object.values(args);
        } else {
            positionalArgs = [];
        }

        try {
            return await wails.Call.ByName(wailsMethod, ...positionalArgs);
        } catch (err) {
            // Wails wraps errors; extract the message
            const msg = typeof err === 'string' ? err : (err.message || String(err));
            throw msg;
        }
    }

    function listen(eventName, callback) {
        const cancel = wails.Events.On(eventName, function(event) {
            // Wails CustomEvent has .data array; Tauri passes {payload: data}
            const payload = event.data && event.data.length > 0 ? event.data[0] : null;
            callback({ payload });
        });
        // Return a promise that resolves to an unlisten function (Tauri convention)
        return Promise.resolve(cancel);
    }

    // Provide __TAURI__ compatibility
    window.__TAURI__ = {
        core: { invoke },
        event: { listen },
        app: {
            getVersion: function() {
                return Promise.resolve('2.0.0-go');
            }
        }
    };

    console.log('[wails-adapter] Tauri API shim loaded');
})();
