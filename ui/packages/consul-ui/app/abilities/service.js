import BaseAbility from './base';

export default class ServiceAbility extends BaseAbility {
  resource = 'service';

  get isLinkable() {
    return this.item.InstanceCount > 0;
  }

  get canReadIntentions() {
    const found = this.item.Resources.find(item => item.Resource === 'intention' && item.Access === 'read' && item.Allow === true);
    return typeof found !== 'undefined';
  }

  get canWriteIntentions() {
    const found = this.item.Resources.find(item => item.Resource === 'intention' && item.Access === 'write' && item.Allow === true);
    return typeof found !== 'undefined';
  }

  get canCreateIntentions() {
    return this.canWriteIntentions;
  }

}
