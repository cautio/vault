Demo.DemoController = Ember.ObjectController.extend({
  currentText: "vault help",
  currentLog: [],
  logPrefix: "$ ",
  cursor: 0,
  notCleared: true,
  isLoading: false,

  setFromHistory: function() {
    var index = this.get('currentLog.length') + this.get('cursor');

    this.set('currentText', this.get('currentLog')[index]);
  }.observes('cursor'),

  actions: {
    close: function() {
      this.transitionTo('index');
    },
  }
});