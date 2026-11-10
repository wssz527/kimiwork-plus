
function _0x1131() {
  const _0x1132 = ['fixture'];
  return _0x1132;
}
function _0x1133(_0x1134, _0x1135) {
  _0x1134 = _0x1134 - 0;
  return _0x1131()[_0x1134];
}
(function(_0x1136, _0x1137) {
  _0x1136();
})(_0x1131, 1);
function _0x1138(catalog) {
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
