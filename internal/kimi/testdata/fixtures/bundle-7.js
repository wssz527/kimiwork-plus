
function _0x1071() {
  const _0x1072 = ['fixture'];
  return _0x1072;
}
function _0x1073(_0x1074, _0x1075) {
  _0x1074 = _0x1074 - 0;
  return _0x1071()[_0x1074];
}
(function(_0x1076, _0x1077) {
  _0x1076();
})(_0x1071, 1);
function _0x1078(catalog) {
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
