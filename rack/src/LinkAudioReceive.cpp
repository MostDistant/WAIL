#include "plugin.hpp"
#include "linkbridge_link.h"


// Link Audio Receive — subscribes to a remote Link Audio channel and plays it
// into Rack. M1 status: joins the session as a Link Audio peer (so it appears
// in the room and can enumerate channels); the PCM path — channel discovery and
// selection via lb_channels, lb_source_create/pop, and 48 kHz -> engine-rate
// conversion — is Milestone 2. process() outputs silence for now.
struct LinkAudioReceive : Module {
	enum ParamId {
		CHANNEL_PARAM,
		PARAMS_LEN
	};
	enum InputId {
		INPUTS_LEN
	};
	enum OutputId {
		OUT_L_OUTPUT,
		OUT_R_OUTPUT,
		OUTPUTS_LEN
	};
	enum LightId {
		SUBSCRIBED_LIGHT,
		LIGHTS_LEN
	};

	lb_link* link = nullptr;
	lb_source* source = nullptr; // created on channel selection (M2)

	LinkAudioReceive() {
		config(PARAMS_LEN, INPUTS_LEN, OUTPUTS_LEN, LIGHTS_LEN);
		configParam(CHANNEL_PARAM, 0.f, 15.f, 0.f, "Channel")->snapEnabled = true;
		configOutput(OUT_L_OUTPUT, "Left");
		configOutput(OUT_R_OUTPUT, "Right");

		link = lb_create(120.0);
		lb_enable(link, true);
		lb_enable_audio(link, true);
	}

	~LinkAudioReceive() override {
		if (source)
			lb_source_destroy(source);
		if (link)
			lb_destroy(link);
	}

	void process(const ProcessArgs& args) override {
		// M2: enumerate channels (lb_channels), (re)subscribe the selected one
		// (lb_source_create) off the audio thread, pop decoded int16 buffers
		// (lb_source_pop) into a ring, and sample-rate-convert 48 kHz ->
		// args.sampleRate here into the two outputs.
		outputs[OUT_L_OUTPUT].setVoltage(0.f);
		outputs[OUT_R_OUTPUT].setVoltage(0.f);
		lights[SUBSCRIBED_LIGHT].setBrightness(source ? 1.f : 0.f);
	}
};


struct LinkAudioReceiveWidget : ModuleWidget {
	LinkAudioReceiveWidget(LinkAudioReceive* module) {
		setModule(module);
		setPanel(createPanel(asset::plugin(pluginInstance, "res/LinkAudioReceive.svg")));

		addChild(createWidget<ScrewSilver>(Vec(RACK_GRID_WIDTH, 0)));
		addChild(createWidget<ScrewSilver>(Vec(box.size.x - 2 * RACK_GRID_WIDTH, 0)));
		addChild(createWidget<ScrewSilver>(Vec(RACK_GRID_WIDTH, RACK_GRID_HEIGHT - RACK_GRID_WIDTH)));
		addChild(createWidget<ScrewSilver>(Vec(box.size.x - 2 * RACK_GRID_WIDTH, RACK_GRID_HEIGHT - RACK_GRID_WIDTH)));

		addParam(createParamCentered<RoundBlackKnob>(mm2px(Vec(20.32, 34.0)), module, LinkAudioReceive::CHANNEL_PARAM));

		addOutput(createOutputCentered<PJ301MPort>(mm2px(Vec(13.0, 108.0)), module, LinkAudioReceive::OUT_L_OUTPUT));
		addOutput(createOutputCentered<PJ301MPort>(mm2px(Vec(27.64, 108.0)), module, LinkAudioReceive::OUT_R_OUTPUT));

		addChild(createLightCentered<SmallLight<GreenLight>>(mm2px(Vec(20.32, 15.0)), module, LinkAudioReceive::SUBSCRIBED_LIGHT));
	}
};


Model* modelLinkAudioReceive = createModel<LinkAudioReceive, LinkAudioReceiveWidget>("LinkAudioReceive");
