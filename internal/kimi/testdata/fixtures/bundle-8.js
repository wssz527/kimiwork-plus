
function _0x1081() {
  const _0x1082 = ['fixture'];
  return _0x1082;
}
function _0x1083(_0x1084, _0x1085) {
  _0x1084 = _0x1084 - 0;
  return _0x1081()[_0x1084];
}
(function(_0x1086, _0x1087) {
  _0x1086();
})(_0x1081, 1);
function _0x1088(catalog) {
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
