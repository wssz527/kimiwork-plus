
function _0x1011() {
  const _0x1012 = ['fixture'];
  return _0x1012;
}
function _0x1013(_0x1014, _0x1015) {
  _0x1014 = _0x1014 - 0;
  return _0x1011()[_0x1014];
}
(function(_0x1016, _0x1017) {
  _0x1016();
})(_0x1011, 1);
function _0x1018(catalog) {
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
