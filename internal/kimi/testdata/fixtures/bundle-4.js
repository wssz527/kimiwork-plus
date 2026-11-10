
function _0x1041() {
  const _0x1042 = ['fixture'];
  return _0x1042;
}
function _0x1043(_0x1044, _0x1045) {
  _0x1044 = _0x1044 - 0;
  return _0x1041()[_0x1044];
}
(function(_0x1046, _0x1047) {
  _0x1046();
})(_0x1041, 1);
function _0x1048(catalog) {
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
