#include "plugin.hpp"
#include "linkbridge_link.h"


// Link Audio Send — publishes a Rack stereo input as a Link Audio channel that
// other Link Audio peers (WAIL, another Most Distant Link Audio Receive, ...)
// can hear. M1 status: joins the session and creates a named Link Audio sink,
// so the channel is announced on the network. The commit path — buffering Rack
// audio into 48 kHz blocks and lb_sink_commit() with the current beat/quantum —
// is Milestone 2.
struct LinkAudioSend : Module {
	enum ParamId {
		GAIN_PARAM,
		PARAMS_LEN
	};
	enum InputId {
		IN_L_INPUT,
		IN_R_INPUT,
		INPUTS_LEN
	};
	enum OutputId {
		OUTPUTS_LEN
	};
	enum LightId {
		ACTIVE_LIGHT,
		LIGHTS_LEN
	};

	lb_link* link = nullptr;
	lb_sink* sink = nullptr;

	LinkAudioSend() {
		config(PARAMS_LEN, INPUTS_LEN, OUTPUTS_LEN, LIGHTS_LEN);
		configParam(GAIN_PARAM, 0.f, 2.f, 1.f, "Gain", "x");
		configInput(IN_L_INPUT, "Left");
		configInput(IN_R_INPUT, "Right");

		link = lb_create(120.0);
		lb_enable(link, true);
		lb_enable_audio(link, true);
		// Announce the channel now; the commit path is M2.
		sink = lb_sink_create(link, "Most Distant", 1 << 15);
	}

	~LinkAudioSend() override {
		if (sink)
			lb_sink_destroy(sink);
		if (link)
			lb_destroy(link);
	}

	void process(const ProcessArgs& args) override {
		// M2: accumulate (gain * inputs) into a 48 kHz block, capture the Link
		// beat/quantum, and lb_sink_commit() each full block. Read the inputs
		// now so the signal intent is explicit while the commit path is stubbed.
		float gain = params[GAIN_PARAM].getValue();
		float l = inputs[IN_L_INPUT].getVoltage() * gain;
		float r = inputs[IN_R_INPUT].getVoltage() * gain;
		(void) l;
		(void) r;
		lights[ACTIVE_LIGHT].setBrightness(sink ? 1.f : 0.f);
	}
};


struct LinkAudioSendWidget : ModuleWidget {
	LinkAudioSendWidget(LinkAudioSend* module) {
		setModule(module);
		setPanel(createPanel(asset::plugin(pluginInstance, "res/LinkAudioSend.svg")));

		addChild(createWidget<ScrewSilver>(Vec(RACK_GRID_WIDTH, 0)));
		addChild(createWidget<ScrewSilver>(Vec(box.size.x - 2 * RACK_GRID_WIDTH, 0)));
		addChild(createWidget<ScrewSilver>(Vec(RACK_GRID_WIDTH, RACK_GRID_HEIGHT - RACK_GRID_WIDTH)));
		addChild(createWidget<ScrewSilver>(Vec(box.size.x - 2 * RACK_GRID_WIDTH, RACK_GRID_HEIGHT - RACK_GRID_WIDTH)));

		addParam(createParamCentered<RoundBlackKnob>(mm2px(Vec(20.32, 34.0)), module, LinkAudioSend::GAIN_PARAM));

		addInput(createInputCentered<PJ301MPort>(mm2px(Vec(13.0, 108.0)), module, LinkAudioSend::IN_L_INPUT));
		addInput(createInputCentered<PJ301MPort>(mm2px(Vec(27.64, 108.0)), module, LinkAudioSend::IN_R_INPUT));

		addChild(createLightCentered<SmallLight<GreenLight>>(mm2px(Vec(20.32, 15.0)), module, LinkAudioSend::ACTIVE_LIGHT));
	}
};


Model* modelLinkAudioSend = createModel<LinkAudioSend, LinkAudioSendWidget>("LinkAudioSend");
