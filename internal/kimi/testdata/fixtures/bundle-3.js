
function _0x1031() {
  const _0x1032 = ['fixture'];
  return _0x1032;
}
function _0x1033(_0x1034, _0x1035) {
  _0x1034 = _0x1034 - 0;
  return _0x1031()[_0x1034];
}
(function(_0x1036, _0x1037) {
  _0x1036();
})(_0x1031, 1);
function _0x1038(catalog) {
  const models = [];
  const skippedModels = [];
  for (const group of Object.values(catalog)) {
    for (const [id, value] of Object.entries(group.models)) {
      if (id.includes('reject')) { skippedModels.push(id); continue; }
      const name = id.toLowerCase().split('/').pop();
      models.push({ model: id, family: name.startsWith('kimi-') ? 'kimi' : name.startsWith('gpt-') ? 'gpt' : 'generic', maxContextSize: value.limit.context });
    }
  }
  return { models, skippedModels };
}
// byok-availability.json byok-providers.json models/api.json
