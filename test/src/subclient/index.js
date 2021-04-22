let { ApiPromise, WsProvider, Keyring } = require('@polkadot/api');
let { bundle } = require("@snowfork/snowbridge-types");
const { default: BigNumber } = require('bignumber.js');

class SubClient {

  constructor(endpoint) {
    this.endpoint = endpoint;
    this.api = null;
    this.keyring = null;
  }

  async connect() {
    const provider = new WsProvider(this.endpoint);
    this.api = await ApiPromise.create({
      provider,
      typesBundle: bundle
    })

    this.keyring = new Keyring({ type: 'sr25519' });
    this.alice = this.keyring.addFromUri('//Alice', { name: 'Alice' });
  }

  async queryAssetBalance(accountId, assetId) {
    let balance = await this.api.query.assets.balances(assetId, accountId);
    return BigNumber(balance.toBigInt())
  }

  async queryAccountBalance(accountId) {
    let {
      data: {
        free: balance
      }
    } = await this.api.query.system.account(accountId);
    return BigNumber(balance.toBigInt())
  }

  async queryNextEventData({ eventSection, eventMethod, eventDataType }) {
    let unsubscribe;
    let foundData = new Promise(async (resolve, reject) => {
      unsubscribe = await this.api.query.system.events((events) => {
        events.forEach((record) => {
          const { event, phase } = record;
          const types = event.typeDef;
          if (event.section === eventSection && event.method === eventMethod) {
            if (eventDataType === undefined) {
              resolve(event.data);
            } else {
              event.data.forEach((data, index) => {
                if (types[index].type === eventDataType) {
                  resolve(data);
                }
              });
            }
          }
        });
      });
    });
    return foundData.then(data => {
      unsubscribe();
      return data;
    })
  }

  async burnETH(account, recipient, amount, channel) {
    return await this.api.tx.eth.burn(channel, recipient, amount).signAndSend(account);
  }

  async burnERC20(account, assetId, recipient, amount, channel) {
    return await this.api.tx.erc20.burn(channel, assetId, recipient, amount).signAndSend(account);
  }

  async lockDOT(account, recipient, amount, channel) {
    return await this.api.tx.dot.lock(channel, recipient, amount).signAndSend(account);
  }

}

module.exports.SubClient = SubClient;
