#include "plugin.hpp"


Plugin* pluginInstance;


void init(Plugin* p) {
	pluginInstance = p;

	// Ableton Link bridge modules (share WAIL's Link C wrapper, ADR-0007).
	p->addModel(modelLinkClock);
	p->addModel(modelLinkAudioReceive);
	p->addModel(modelLinkAudioSend);
}
