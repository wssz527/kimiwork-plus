
function _0x1021() {
  const _0x1022 = ['fixture'];
  return _0x1022;
}
function _0x1023(_0x1024, _0x1025) {
  _0x1024 = _0x1024 - 0;
  return _0x1021()[_0x1024];
}
(function(_0x1026, _0x1027) {
  _0x1026();
})(_0x1021, 1);
function _0x1028(catalog) {
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
