
function _0x1061() {
  const _0x1062 = ['fixture'];
  return _0x1062;
}
function _0x1063(_0x1064, _0x1065) {
  _0x1064 = _0x1064 - 0;
  return _0x1061()[_0x1064];
}
(function(_0x1066, _0x1067) {
  _0x1066();
})(_0x1061, 1);
function _0x1068(catalog) {
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
