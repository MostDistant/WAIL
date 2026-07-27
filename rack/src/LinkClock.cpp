#include "plugin.hpp"
#include "linkbridge_link.h"

#include <algorithm>
#include <cmath>


// Link Clock — a Most Distant module that joins the local Ableton Link session
// as a peer and turns its shared timeline into CV: a beat clock, a bar reset,
// and a run gate driven by Link start/stop transport.
//
// M1 status: this is a real Link peer (the session is created on the main
// thread in the constructor, matching the lb_ API threading contract) and
// derives clock/reset/run from the Link timeline. The session snapshot is
// throttled by a ClockDivider because lb_capture() allocates and must not run
// per-sample on the audio thread; M2 should replace it with a preallocated,
// realtime-safe capture (WAIL ADR-0002) and interpolate the beat between
// snapshots for sample-accurate edges.
struct LinkClock : Module {
	enum ParamId {
		PPQN_PARAM,
		QUANTUM_PARAM,
		PARAMS_LEN
	};
	enum InputId {
		INPUTS_LEN
	};
	enum OutputId {
		CLOCK_OUTPUT,
		RESET_OUTPUT,
		RUN_OUTPUT,
		OUTPUTS_LEN
	};
	enum LightId {
		PLAYING_LIGHT,
		PEER_LIGHT,
		LIGHTS_LEN
	};

	lb_link* link = nullptr;
	dsp::ClockDivider snapDivider; // throttles Link captures off the per-sample path
	dsp::PulseGenerator clockPulse;
	dsp::PulseGenerator resetPulse;
	double lastBeat = -1.0;
	bool playing = false;
	int peers = 0;

	LinkClock() {
		config(PARAMS_LEN, INPUTS_LEN, OUTPUTS_LEN, LIGHTS_LEN);
		configParam(PPQN_PARAM, 1.f, 24.f, 4.f, "Pulses per beat")->snapEnabled = true;
		configParam(QUANTUM_PARAM, 1.f, 16.f, 4.f, "Quantum (beats per bar)")->snapEnabled = true;
		configOutput(CLOCK_OUTPUT, "Clock");
		configOutput(RESET_OUTPUT, "Bar reset");
		configOutput(RUN_OUTPUT, "Run (Link transport)");

		snapDivider.setDivision(64);

		// One abl_link handle = one LAN peer. Start/stop sync on so RUN follows
		// the room's transport.
		link = lb_create(120.0);
		lb_enable(link, true);
		lb_enable_start_stop_sync(link, true);
	}

	~LinkClock() override {
		if (link)
			lb_destroy(link);
	}

	void process(const ProcessArgs& args) override {
		if (!link)
			return;

		const double quantum = std::max(1.0, std::round(params[QUANTUM_PARAM].getValue()));
		const double ppqn = std::max(1.0, std::round(params[PPQN_PARAM].getValue()));

		// Snapshot the Link timeline at a reduced rate (see header note).
		if (snapDivider.process()) {
			lb_state* st = lb_capture(link);
			const int64_t now = lb_clock_micros(link);
			const double beat = lb_beat_at_time(st, now, quantum);
			playing = lb_state_is_playing(st);
			lb_release(st);
			peers = (int) lb_num_peers(link);

			if (playing && lastBeat >= 0.0) {
				// Clock pulse on each 1/ppqn subdivision boundary crossed.
				if (std::floor(beat * ppqn) > std::floor(lastBeat * ppqn))
					clockPulse.trigger(1e-3f);
				// Reset pulse at each bar (quantum) boundary crossed.
				if (std::floor(beat / quantum) > std::floor(lastBeat / quantum))
					resetPulse.trigger(1e-3f);
			}
			lastBeat = beat;
		}

		outputs[CLOCK_OUTPUT].setVoltage(clockPulse.process(args.sampleTime) ? 10.f : 0.f);
		outputs[RESET_OUTPUT].setVoltage(resetPulse.process(args.sampleTime) ? 10.f : 0.f);
		outputs[RUN_OUTPUT].setVoltage(playing ? 10.f : 0.f);

		lights[PLAYING_LIGHT].setBrightness(playing ? 1.f : 0.f);
		lights[PEER_LIGHT].setBrightness(peers > 0 ? 1.f : 0.f);
	}
};


struct LinkClockWidget : ModuleWidget {
	LinkClockWidget(LinkClock* module) {
		setModule(module);
		setPanel(createPanel(asset::plugin(pluginInstance, "res/LinkClock.svg")));

		addChild(createWidget<ScrewSilver>(Vec(RACK_GRID_WIDTH, 0)));
		addChild(createWidget<ScrewSilver>(Vec(box.size.x - 2 * RACK_GRID_WIDTH, 0)));
		addChild(createWidget<ScrewSilver>(Vec(RACK_GRID_WIDTH, RACK_GRID_HEIGHT - RACK_GRID_WIDTH)));
		addChild(createWidget<ScrewSilver>(Vec(box.size.x - 2 * RACK_GRID_WIDTH, RACK_GRID_HEIGHT - RACK_GRID_WIDTH)));

		addParam(createParamCentered<RoundBlackKnob>(mm2px(Vec(20.32, 30.0)), module, LinkClock::PPQN_PARAM));
		addParam(createParamCentered<RoundBlackKnob>(mm2px(Vec(20.32, 50.0)), module, LinkClock::QUANTUM_PARAM));

		addOutput(createOutputCentered<PJ301MPort>(mm2px(Vec(20.32, 80.0)), module, LinkClock::CLOCK_OUTPUT));
		addOutput(createOutputCentered<PJ301MPort>(mm2px(Vec(20.32, 98.0)), module, LinkClock::RESET_OUTPUT));
		addOutput(createOutputCentered<PJ301MPort>(mm2px(Vec(20.32, 116.0)), module, LinkClock::RUN_OUTPUT));

		addChild(createLightCentered<SmallLight<GreenLight>>(mm2px(Vec(15.0, 15.0)), module, LinkClock::PLAYING_LIGHT));
		addChild(createLightCentered<SmallLight<YellowLight>>(mm2px(Vec(25.64, 15.0)), module, LinkClock::PEER_LIGHT));
	}
};


Model* modelLinkClock = createModel<LinkClock, LinkClockWidget>("LinkClock");
