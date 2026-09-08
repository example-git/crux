var __defProp = Object.defineProperty;
var __defProps = Object.defineProperties;
var __getOwnPropDescs = Object.getOwnPropertyDescriptors;
var __getOwnPropSymbols = Object.getOwnPropertySymbols;
var __hasOwnProp = Object.prototype.hasOwnProperty;
var __propIsEnum = Object.prototype.propertyIsEnumerable;
var __knownSymbol = (name, symbol) => (symbol = Symbol[name]) ? symbol : /* @__PURE__ */ Symbol.for("Symbol." + name);
var __typeError = (msg) => {
  throw TypeError(msg);
};
var __pow = Math.pow;
var __defNormalProp = (obj, key, value) => key in obj ? __defProp(obj, key, { enumerable: true, configurable: true, writable: true, value }) : obj[key] = value;
var __spreadValues = (a, b) => {
  for (var prop in b || (b = {}))
    if (__hasOwnProp.call(b, prop))
      __defNormalProp(a, prop, b[prop]);
  if (__getOwnPropSymbols)
    for (var prop of __getOwnPropSymbols(b)) {
      if (__propIsEnum.call(b, prop))
        __defNormalProp(a, prop, b[prop]);
    }
  return a;
};
var __spreadProps = (a, b) => __defProps(a, __getOwnPropDescs(b));
var __async = (__this, __arguments, generator) => {
  return new Promise((resolve, reject) => {
    var fulfilled = (value) => {
      try {
        step(generator.next(value));
      } catch (e) {
        reject(e);
      }
    };
    var rejected = (value) => {
      try {
        step(generator.throw(value));
      } catch (e) {
        reject(e);
      }
    };
    var step = (x) => x.done ? resolve(x.value) : Promise.resolve(x.value).then(fulfilled, rejected);
    step((generator = generator.apply(__this, __arguments)).next());
  });
};
var __await = function(promise, isYieldStar) {
  this[0] = promise;
  this[1] = isYieldStar;
};
var __yieldStar = (value) => {
  var obj = value[__knownSymbol("asyncIterator")], isAwait = false, method, it = {};
  if (obj == null) {
    obj = value[__knownSymbol("iterator")]();
    method = (k) => it[k] = (x) => obj[k](x);
  } else {
    obj = obj.call(value);
    method = (k) => it[k] = (v) => {
      if (isAwait) {
        isAwait = false;
        if (k === "throw") throw v;
        return v;
      }
      isAwait = true;
      return {
        done: false,
        value: new __await(new Promise((resolve) => {
          var x = obj[k](v);
          if (!(x instanceof Object)) __typeError("Object expected");
          resolve(x);
        }), 1)
      };
    };
  }
  return it[__knownSymbol("iterator")] = () => it, method("next"), "throw" in obj ? method("throw") : it.throw = (x) => {
    throw x;
  }, "return" in obj && method("return"), it;
};
var __forAwait = (obj, it, method) => (it = obj[__knownSymbol("asyncIterator")]) ? it.call(obj) : (obj = obj[__knownSymbol("iterator")](), it = {}, method = (key, fn) => (fn = obj[key]) && (it[key] = (arg) => new Promise((yes, no, done) => (arg = fn.call(obj, arg), done = arg.done, Promise.resolve(arg.value).then((value) => yes({ value, done }), no)))), method("next"), method("return"), it);
(() => {
  "use strict";
  var e = { 5639: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.CircularList = void 0;
    const i2 = s2(7150), r2 = s2(802);
    class n2 extends i2.Disposable {
      constructor(e3) {
        super(), this._maxLength = e3, this.onDeleteEmitter = this._register(new r2.Emitter()), this.onDelete = this.onDeleteEmitter.event, this.onInsertEmitter = this._register(new r2.Emitter()), this.onInsert = this.onInsertEmitter.event, this.onTrimEmitter = this._register(new r2.Emitter()), this.onTrim = this.onTrimEmitter.event, this._array = new Array(this._maxLength), this._startIndex = 0, this._length = 0;
      }
      get maxLength() {
        return this._maxLength;
      }
      set maxLength(e3) {
        if (this._maxLength === e3) return;
        const t3 = new Array(e3);
        for (let s3 = 0; s3 < Math.min(e3, this.length); s3++) t3[s3] = this._array[this._getCyclicIndex(s3)];
        this._array = t3, this._maxLength = e3, this._startIndex = 0;
      }
      get length() {
        return this._length;
      }
      set length(e3) {
        if (e3 > this._length) for (let t3 = this._length; t3 < e3; t3++) this._array[t3] = void 0;
        this._length = e3;
      }
      get(e3) {
        return this._array[this._getCyclicIndex(e3)];
      }
      set(e3, t3) {
        this._array[this._getCyclicIndex(e3)] = t3;
      }
      push(e3) {
        this._array[this._getCyclicIndex(this._length)] = e3, this._length === this._maxLength ? (this._startIndex = ++this._startIndex % this._maxLength, this.onTrimEmitter.fire(1)) : this._length++;
      }
      recycle() {
        if (this._length !== this._maxLength) throw new Error("Can only recycle when the buffer is full");
        return this._startIndex = ++this._startIndex % this._maxLength, this.onTrimEmitter.fire(1), this._array[this._getCyclicIndex(this._length - 1)];
      }
      get isFull() {
        return this._length === this._maxLength;
      }
      pop() {
        return this._array[this._getCyclicIndex(this._length-- - 1)];
      }
      splice(e3, t3, ...s3) {
        if (t3) {
          for (let s4 = e3; s4 < this._length - t3; s4++) this._array[this._getCyclicIndex(s4)] = this._array[this._getCyclicIndex(s4 + t3)];
          this._length -= t3, this.onDeleteEmitter.fire({ index: e3, amount: t3 });
        }
        for (let t4 = this._length - 1; t4 >= e3; t4--) this._array[this._getCyclicIndex(t4 + s3.length)] = this._array[this._getCyclicIndex(t4)];
        for (let t4 = 0; t4 < s3.length; t4++) this._array[this._getCyclicIndex(e3 + t4)] = s3[t4];
        if (s3.length && this.onInsertEmitter.fire({ index: e3, amount: s3.length }), this._length + s3.length > this._maxLength) {
          const e4 = this._length + s3.length - this._maxLength;
          this._startIndex += e4, this._length = this._maxLength, this.onTrimEmitter.fire(e4);
        } else this._length += s3.length;
      }
      trimStart(e3) {
        e3 > this._length && (e3 = this._length), this._startIndex += e3, this._length -= e3, this.onTrimEmitter.fire(e3);
      }
      shiftElements(e3, t3, s3) {
        if (!(t3 <= 0)) {
          if (e3 < 0 || e3 >= this._length) throw new Error("start argument out of range");
          if (e3 + s3 < 0) throw new Error("Cannot shift elements in list beyond index 0");
          if (s3 > 0) {
            for (let i4 = t3 - 1; i4 >= 0; i4--) this.set(e3 + i4 + s3, this.get(e3 + i4));
            const i3 = e3 + t3 + s3 - this._length;
            if (i3 > 0) for (this._length += i3; this._length > this._maxLength; ) this._length--, this._startIndex++, this.onTrimEmitter.fire(1);
          } else for (let i3 = 0; i3 < t3; i3++) this.set(e3 + i3 + s3, this.get(e3 + i3));
        }
      }
      _getCyclicIndex(e3) {
        return (this._startIndex + e3) % this._maxLength;
      }
    }
    t2.CircularList = n2;
  }, 7453: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.clone = function e3(t3, s2 = 5) {
      if ("object" != typeof t3) return t3;
      const i2 = Array.isArray(t3) ? [] : {};
      for (const r2 in t3) i2[r2] = s2 <= 1 ? t3[r2] : t3[r2] && e3(t3[r2], s2 - 1);
      return i2;
    };
  }, 5777: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.CoreTerminal = void 0;
    const i2 = s2(6501), r2 = s2(6025), n2 = s2(7276), o = s2(9640), a = s2(56), h = s2(4071), c = s2(7792), l = s2(6415), u = s2(5746), d = s2(5882), f = s2(2486), _ = s2(3562), p = s2(8811), g = s2(802), v = s2(7150);
    let m = false;
    class b extends v.Disposable {
      get onScroll() {
        return this._onScrollApi || (this._onScrollApi = this._register(new g.Emitter()), this._onScroll.event(((e3) => {
          var _a;
          (_a = this._onScrollApi) == null ? void 0 : _a.fire(e3.position);
        }))), this._onScrollApi.event;
      }
      get cols() {
        return this._bufferService.cols;
      }
      get rows() {
        return this._bufferService.rows;
      }
      get buffers() {
        return this._bufferService.buffers;
      }
      get options() {
        return this.optionsService.options;
      }
      set options(e3) {
        for (const t3 in e3) this.optionsService.options[t3] = e3[t3];
      }
      constructor(e3) {
        super(), this._windowsWrappingHeuristics = this._register(new v.MutableDisposable()), this._onBinary = this._register(new g.Emitter()), this.onBinary = this._onBinary.event, this._onData = this._register(new g.Emitter()), this.onData = this._onData.event, this._onLineFeed = this._register(new g.Emitter()), this.onLineFeed = this._onLineFeed.event, this._onResize = this._register(new g.Emitter()), this.onResize = this._onResize.event, this._onWriteParsed = this._register(new g.Emitter()), this.onWriteParsed = this._onWriteParsed.event, this._onScroll = this._register(new g.Emitter()), this._instantiationService = new r2.InstantiationService(), this.optionsService = this._register(new a.OptionsService(e3)), this._instantiationService.setService(i2.IOptionsService, this.optionsService), this._bufferService = this._register(this._instantiationService.createInstance(o.BufferService)), this._instantiationService.setService(i2.IBufferService, this._bufferService), this._logService = this._register(this._instantiationService.createInstance(n2.LogService)), this._instantiationService.setService(i2.ILogService, this._logService), this.coreService = this._register(this._instantiationService.createInstance(h.CoreService)), this._instantiationService.setService(i2.ICoreService, this.coreService), this.coreMouseService = this._register(this._instantiationService.createInstance(c.CoreMouseService)), this._instantiationService.setService(i2.ICoreMouseService, this.coreMouseService), this.unicodeService = this._register(this._instantiationService.createInstance(l.UnicodeService)), this._instantiationService.setService(i2.IUnicodeService, this.unicodeService), this._charsetService = this._instantiationService.createInstance(u.CharsetService), this._instantiationService.setService(i2.ICharsetService, this._charsetService), this._oscLinkService = this._instantiationService.createInstance(p.OscLinkService), this._instantiationService.setService(i2.IOscLinkService, this._oscLinkService), this._inputHandler = this._register(new f.InputHandler(this._bufferService, this._charsetService, this.coreService, this._logService, this.optionsService, this._oscLinkService, this.coreMouseService, this.unicodeService)), this._register(g.Event.forward(this._inputHandler.onLineFeed, this._onLineFeed)), this._register(this._inputHandler), this._register(g.Event.forward(this._bufferService.onResize, this._onResize)), this._register(g.Event.forward(this.coreService.onData, this._onData)), this._register(g.Event.forward(this.coreService.onBinary, this._onBinary)), this._register(this.coreService.onRequestScrollToBottom((() => this.scrollToBottom(true)))), this._register(this.coreService.onUserInput((() => this._writeBuffer.handleUserInput()))), this._register(this.optionsService.onMultipleOptionChange(["windowsMode", "windowsPty"], (() => this._handleWindowsPtyOptionChange()))), this._register(this._bufferService.onScroll((() => {
          this._onScroll.fire({ position: this._bufferService.buffer.ydisp }), this._inputHandler.markRangeDirty(this._bufferService.buffer.scrollTop, this._bufferService.buffer.scrollBottom);
        }))), this._writeBuffer = this._register(new _.WriteBuffer(((e4, t3) => this._inputHandler.parse(e4, t3)))), this._register(g.Event.forward(this._writeBuffer.onWriteParsed, this._onWriteParsed));
      }
      write(e3, t3) {
        this._writeBuffer.write(e3, t3);
      }
      writeSync(e3, t3) {
        this._logService.logLevel <= i2.LogLevelEnum.WARN && !m && (this._logService.warn("writeSync is unreliable and will be removed soon."), m = true), this._writeBuffer.writeSync(e3, t3);
      }
      input(e3, t3 = true) {
        this.coreService.triggerDataEvent(e3, t3);
      }
      resize(e3, t3) {
        isNaN(e3) || isNaN(t3) || (e3 = Math.max(e3, o.MINIMUM_COLS), t3 = Math.max(t3, o.MINIMUM_ROWS), this._bufferService.resize(e3, t3));
      }
      scroll(e3, t3 = false) {
        this._bufferService.scroll(e3, t3);
      }
      scrollLines(e3, t3) {
        this._bufferService.scrollLines(e3, t3);
      }
      scrollPages(e3) {
        this.scrollLines(e3 * (this.rows - 1));
      }
      scrollToTop() {
        this.scrollLines(-this._bufferService.buffer.ydisp);
      }
      scrollToBottom(e3) {
        this.scrollLines(this._bufferService.buffer.ybase - this._bufferService.buffer.ydisp);
      }
      scrollToLine(e3) {
        const t3 = e3 - this._bufferService.buffer.ydisp;
        0 !== t3 && this.scrollLines(t3);
      }
      registerEscHandler(e3, t3) {
        return this._inputHandler.registerEscHandler(e3, t3);
      }
      registerDcsHandler(e3, t3) {
        return this._inputHandler.registerDcsHandler(e3, t3);
      }
      registerCsiHandler(e3, t3) {
        return this._inputHandler.registerCsiHandler(e3, t3);
      }
      registerOscHandler(e3, t3) {
        return this._inputHandler.registerOscHandler(e3, t3);
      }
      _setup() {
        this._handleWindowsPtyOptionChange();
      }
      reset() {
        this._inputHandler.reset(), this._bufferService.reset(), this._charsetService.reset(), this.coreService.reset(), this.coreMouseService.reset();
      }
      _handleWindowsPtyOptionChange() {
        let e3 = false;
        const t3 = this.optionsService.rawOptions.windowsPty;
        t3 && void 0 !== t3.buildNumber && void 0 !== t3.buildNumber ? e3 = !!("conpty" === t3.backend && t3.buildNumber < 21376) : this.optionsService.rawOptions.windowsMode && (e3 = true), e3 ? this._enableWindowsWrappingHeuristics() : this._windowsWrappingHeuristics.clear();
      }
      _enableWindowsWrappingHeuristics() {
        if (!this._windowsWrappingHeuristics.value) {
          const e3 = [];
          e3.push(this.onLineFeed(d.updateWindowsModeWrappedState.bind(null, this._bufferService))), e3.push(this.registerCsiHandler({ final: "H" }, (() => ((0, d.updateWindowsModeWrappedState)(this._bufferService), false)))), this._windowsWrappingHeuristics.value = (0, v.toDisposable)((() => {
            for (const t3 of e3) t3.dispose();
          }));
        }
      }
    }
    t2.CoreTerminal = b;
  }, 2486: function(e2, t2, s2) {
    var i2 = this && this.__decorate || function(e3, t3, s3, i3) {
      var r3, n3 = arguments.length, o2 = n3 < 3 ? t3 : null === i3 ? i3 = Object.getOwnPropertyDescriptor(t3, s3) : i3;
      if ("object" == typeof Reflect && "function" == typeof Reflect.decorate) o2 = Reflect.decorate(e3, t3, s3, i3);
      else for (var a2 = e3.length - 1; a2 >= 0; a2--) (r3 = e3[a2]) && (o2 = (n3 < 3 ? r3(o2) : n3 > 3 ? r3(t3, s3, o2) : r3(t3, s3)) || o2);
      return n3 > 3 && o2 && Object.defineProperty(t3, s3, o2), o2;
    }, r2 = this && this.__param || function(e3, t3) {
      return function(s3, i3) {
        t3(s3, i3, e3);
      };
    };
    Object.defineProperty(t2, "__esModule", { value: true }), t2.InputHandler = t2.WindowsOptionsReportType = void 0, t2.isValidColorIndex = k;
    const n2 = s2(3534), o = s2(6760), a = s2(6717), h = s2(7150), c = s2(726), l = s2(6107), u = s2(8938), d = s2(3055), f = s2(5451), _ = s2(6501), p = s2(6415), g = s2(1346), v = s2(9823), m = s2(8693), b = s2(802), S = { "(": 0, ")": 1, "*": 2, "+": 3, "-": 1, ".": 2 }, y = 131072;
    function C(e3, t3) {
      if (e3 > 24) return t3.setWinLines || false;
      switch (e3) {
        case 1:
          return !!t3.restoreWin;
        case 2:
          return !!t3.minimizeWin;
        case 3:
          return !!t3.setWinPosition;
        case 4:
          return !!t3.setWinSizePixels;
        case 5:
          return !!t3.raiseWin;
        case 6:
          return !!t3.lowerWin;
        case 7:
          return !!t3.refreshWin;
        case 8:
          return !!t3.setWinSizeChars;
        case 9:
          return !!t3.maximizeWin;
        case 10:
          return !!t3.fullscreenWin;
        case 11:
          return !!t3.getWinState;
        case 13:
          return !!t3.getWinPosition;
        case 14:
          return !!t3.getWinSizePixels;
        case 15:
          return !!t3.getScreenSizePixels;
        case 16:
          return !!t3.getCellSizePixels;
        case 18:
          return !!t3.getWinSizeChars;
        case 19:
          return !!t3.getScreenSizeChars;
        case 20:
          return !!t3.getIconTitle;
        case 21:
          return !!t3.getWinTitle;
        case 22:
          return !!t3.pushTitle;
        case 23:
          return !!t3.popTitle;
        case 24:
          return !!t3.setWinLines;
      }
      return false;
    }
    var w;
    !(function(e3) {
      e3[e3.GET_WIN_SIZE_PIXELS = 0] = "GET_WIN_SIZE_PIXELS", e3[e3.GET_CELL_SIZE_PIXELS = 1] = "GET_CELL_SIZE_PIXELS";
    })(w || (t2.WindowsOptionsReportType = w = {}));
    let E = 0;
    class A extends h.Disposable {
      getAttrData() {
        return this._curAttrData;
      }
      constructor(e3, t3, s3, i3, r3, h2, u2, d2, f2 = new a.EscapeSequenceParser()) {
        super(), this._bufferService = e3, this._charsetService = t3, this._coreService = s3, this._logService = i3, this._optionsService = r3, this._oscLinkService = h2, this._coreMouseService = u2, this._unicodeService = d2, this._parser = f2, this._parseBuffer = new Uint32Array(4096), this._stringDecoder = new c.StringToUtf32(), this._utf8Decoder = new c.Utf8ToUtf32(), this._windowTitle = "", this._iconName = "", this._windowTitleStack = [], this._iconNameStack = [], this._curAttrData = l.DEFAULT_ATTR_DATA.clone(), this._eraseAttrDataInternal = l.DEFAULT_ATTR_DATA.clone(), this._onRequestBell = this._register(new b.Emitter()), this.onRequestBell = this._onRequestBell.event, this._onRequestRefreshRows = this._register(new b.Emitter()), this.onRequestRefreshRows = this._onRequestRefreshRows.event, this._onRequestReset = this._register(new b.Emitter()), this.onRequestReset = this._onRequestReset.event, this._onRequestSendFocus = this._register(new b.Emitter()), this.onRequestSendFocus = this._onRequestSendFocus.event, this._onRequestSyncScrollBar = this._register(new b.Emitter()), this.onRequestSyncScrollBar = this._onRequestSyncScrollBar.event, this._onRequestWindowsOptionsReport = this._register(new b.Emitter()), this.onRequestWindowsOptionsReport = this._onRequestWindowsOptionsReport.event, this._onA11yChar = this._register(new b.Emitter()), this.onA11yChar = this._onA11yChar.event, this._onA11yTab = this._register(new b.Emitter()), this.onA11yTab = this._onA11yTab.event, this._onCursorMove = this._register(new b.Emitter()), this.onCursorMove = this._onCursorMove.event, this._onLineFeed = this._register(new b.Emitter()), this.onLineFeed = this._onLineFeed.event, this._onScroll = this._register(new b.Emitter()), this.onScroll = this._onScroll.event, this._onTitleChange = this._register(new b.Emitter()), this.onTitleChange = this._onTitleChange.event, this._onColor = this._register(new b.Emitter()), this.onColor = this._onColor.event, this._parseStack = { paused: false, cursorStartX: 0, cursorStartY: 0, decodedLength: 0, position: 0 }, this._specialColors = [256, 257, 258], this._register(this._parser), this._dirtyRowTracker = new L(this._bufferService), this._activeBuffer = this._bufferService.buffer, this._register(this._bufferService.buffers.onBufferActivate(((e4) => this._activeBuffer = e4.activeBuffer))), this._parser.setCsiHandlerFallback(((e4, t4) => {
          this._logService.debug("Unknown CSI code: ", { identifier: this._parser.identToString(e4), params: t4.toArray() });
        })), this._parser.setEscHandlerFallback(((e4) => {
          this._logService.debug("Unknown ESC code: ", { identifier: this._parser.identToString(e4) });
        })), this._parser.setExecuteHandlerFallback(((e4) => {
          this._logService.debug("Unknown EXECUTE code: ", { code: e4 });
        })), this._parser.setOscHandlerFallback(((e4, t4, s4) => {
          this._logService.debug("Unknown OSC code: ", { identifier: e4, action: t4, data: s4 });
        })), this._parser.setDcsHandlerFallback(((e4, t4, s4) => {
          "HOOK" === t4 && (s4 = s4.toArray()), this._logService.debug("Unknown DCS code: ", { identifier: this._parser.identToString(e4), action: t4, payload: s4 });
        })), this._parser.setPrintHandler(((e4, t4, s4) => this.print(e4, t4, s4))), this._parser.registerCsiHandler({ final: "@" }, ((e4) => this.insertChars(e4))), this._parser.registerCsiHandler({ intermediates: " ", final: "@" }, ((e4) => this.scrollLeft(e4))), this._parser.registerCsiHandler({ final: "A" }, ((e4) => this.cursorUp(e4))), this._parser.registerCsiHandler({ intermediates: " ", final: "A" }, ((e4) => this.scrollRight(e4))), this._parser.registerCsiHandler({ final: "B" }, ((e4) => this.cursorDown(e4))), this._parser.registerCsiHandler({ final: "C" }, ((e4) => this.cursorForward(e4))), this._parser.registerCsiHandler({ final: "D" }, ((e4) => this.cursorBackward(e4))), this._parser.registerCsiHandler({ final: "E" }, ((e4) => this.cursorNextLine(e4))), this._parser.registerCsiHandler({ final: "F" }, ((e4) => this.cursorPrecedingLine(e4))), this._parser.registerCsiHandler({ final: "G" }, ((e4) => this.cursorCharAbsolute(e4))), this._parser.registerCsiHandler({ final: "H" }, ((e4) => this.cursorPosition(e4))), this._parser.registerCsiHandler({ final: "I" }, ((e4) => this.cursorForwardTab(e4))), this._parser.registerCsiHandler({ final: "J" }, ((e4) => this.eraseInDisplay(e4, false))), this._parser.registerCsiHandler({ prefix: "?", final: "J" }, ((e4) => this.eraseInDisplay(e4, true))), this._parser.registerCsiHandler({ final: "K" }, ((e4) => this.eraseInLine(e4, false))), this._parser.registerCsiHandler({ prefix: "?", final: "K" }, ((e4) => this.eraseInLine(e4, true))), this._parser.registerCsiHandler({ final: "L" }, ((e4) => this.insertLines(e4))), this._parser.registerCsiHandler({ final: "M" }, ((e4) => this.deleteLines(e4))), this._parser.registerCsiHandler({ final: "P" }, ((e4) => this.deleteChars(e4))), this._parser.registerCsiHandler({ final: "S" }, ((e4) => this.scrollUp(e4))), this._parser.registerCsiHandler({ final: "T" }, ((e4) => this.scrollDown(e4))), this._parser.registerCsiHandler({ final: "X" }, ((e4) => this.eraseChars(e4))), this._parser.registerCsiHandler({ final: "Z" }, ((e4) => this.cursorBackwardTab(e4))), this._parser.registerCsiHandler({ final: "`" }, ((e4) => this.charPosAbsolute(e4))), this._parser.registerCsiHandler({ final: "a" }, ((e4) => this.hPositionRelative(e4))), this._parser.registerCsiHandler({ final: "b" }, ((e4) => this.repeatPrecedingCharacter(e4))), this._parser.registerCsiHandler({ final: "c" }, ((e4) => this.sendDeviceAttributesPrimary(e4))), this._parser.registerCsiHandler({ prefix: ">", final: "c" }, ((e4) => this.sendDeviceAttributesSecondary(e4))), this._parser.registerCsiHandler({ final: "d" }, ((e4) => this.linePosAbsolute(e4))), this._parser.registerCsiHandler({ final: "e" }, ((e4) => this.vPositionRelative(e4))), this._parser.registerCsiHandler({ final: "f" }, ((e4) => this.hVPosition(e4))), this._parser.registerCsiHandler({ final: "g" }, ((e4) => this.tabClear(e4))), this._parser.registerCsiHandler({ final: "h" }, ((e4) => this.setMode(e4))), this._parser.registerCsiHandler({ prefix: "?", final: "h" }, ((e4) => this.setModePrivate(e4))), this._parser.registerCsiHandler({ final: "l" }, ((e4) => this.resetMode(e4))), this._parser.registerCsiHandler({ prefix: "?", final: "l" }, ((e4) => this.resetModePrivate(e4))), this._parser.registerCsiHandler({ final: "m" }, ((e4) => this.charAttributes(e4))), this._parser.registerCsiHandler({ final: "n" }, ((e4) => this.deviceStatus(e4))), this._parser.registerCsiHandler({ prefix: "?", final: "n" }, ((e4) => this.deviceStatusPrivate(e4))), this._parser.registerCsiHandler({ intermediates: "!", final: "p" }, ((e4) => this.softReset(e4))), this._parser.registerCsiHandler({ intermediates: " ", final: "q" }, ((e4) => this.setCursorStyle(e4))), this._parser.registerCsiHandler({ final: "r" }, ((e4) => this.setScrollRegion(e4))), this._parser.registerCsiHandler({ final: "s" }, ((e4) => this.saveCursor(e4))), this._parser.registerCsiHandler({ final: "t" }, ((e4) => this.windowOptions(e4))), this._parser.registerCsiHandler({ final: "u" }, ((e4) => this.restoreCursor(e4))), this._parser.registerCsiHandler({ intermediates: "'", final: "}" }, ((e4) => this.insertColumns(e4))), this._parser.registerCsiHandler({ intermediates: "'", final: "~" }, ((e4) => this.deleteColumns(e4))), this._parser.registerCsiHandler({ intermediates: '"', final: "q" }, ((e4) => this.selectProtected(e4))), this._parser.registerCsiHandler({ intermediates: "$", final: "p" }, ((e4) => this.requestMode(e4, true))), this._parser.registerCsiHandler({ prefix: "?", intermediates: "$", final: "p" }, ((e4) => this.requestMode(e4, false))), this._parser.setExecuteHandler(n2.C0.BEL, (() => this.bell())), this._parser.setExecuteHandler(n2.C0.LF, (() => this.lineFeed())), this._parser.setExecuteHandler(n2.C0.VT, (() => this.lineFeed())), this._parser.setExecuteHandler(n2.C0.FF, (() => this.lineFeed())), this._parser.setExecuteHandler(n2.C0.CR, (() => this.carriageReturn())), this._parser.setExecuteHandler(n2.C0.BS, (() => this.backspace())), this._parser.setExecuteHandler(n2.C0.HT, (() => this.tab())), this._parser.setExecuteHandler(n2.C0.SO, (() => this.shiftOut())), this._parser.setExecuteHandler(n2.C0.SI, (() => this.shiftIn())), this._parser.setExecuteHandler(n2.C1.IND, (() => this.index())), this._parser.setExecuteHandler(n2.C1.NEL, (() => this.nextLine())), this._parser.setExecuteHandler(n2.C1.HTS, (() => this.tabSet())), this._parser.registerOscHandler(0, new g.OscHandler(((e4) => (this.setTitle(e4), this.setIconName(e4), true)))), this._parser.registerOscHandler(1, new g.OscHandler(((e4) => this.setIconName(e4)))), this._parser.registerOscHandler(2, new g.OscHandler(((e4) => this.setTitle(e4)))), this._parser.registerOscHandler(4, new g.OscHandler(((e4) => this.setOrReportIndexedColor(e4)))), this._parser.registerOscHandler(8, new g.OscHandler(((e4) => this.setHyperlink(e4)))), this._parser.registerOscHandler(10, new g.OscHandler(((e4) => this.setOrReportFgColor(e4)))), this._parser.registerOscHandler(11, new g.OscHandler(((e4) => this.setOrReportBgColor(e4)))), this._parser.registerOscHandler(12, new g.OscHandler(((e4) => this.setOrReportCursorColor(e4)))), this._parser.registerOscHandler(104, new g.OscHandler(((e4) => this.restoreIndexedColor(e4)))), this._parser.registerOscHandler(110, new g.OscHandler(((e4) => this.restoreFgColor(e4)))), this._parser.registerOscHandler(111, new g.OscHandler(((e4) => this.restoreBgColor(e4)))), this._parser.registerOscHandler(112, new g.OscHandler(((e4) => this.restoreCursorColor(e4)))), this._parser.registerEscHandler({ final: "7" }, (() => this.saveCursor())), this._parser.registerEscHandler({ final: "8" }, (() => this.restoreCursor())), this._parser.registerEscHandler({ final: "D" }, (() => this.index())), this._parser.registerEscHandler({ final: "E" }, (() => this.nextLine())), this._parser.registerEscHandler({ final: "H" }, (() => this.tabSet())), this._parser.registerEscHandler({ final: "M" }, (() => this.reverseIndex())), this._parser.registerEscHandler({ final: "=" }, (() => this.keypadApplicationMode())), this._parser.registerEscHandler({ final: ">" }, (() => this.keypadNumericMode())), this._parser.registerEscHandler({ final: "c" }, (() => this.fullReset())), this._parser.registerEscHandler({ final: "n" }, (() => this.setgLevel(2))), this._parser.registerEscHandler({ final: "o" }, (() => this.setgLevel(3))), this._parser.registerEscHandler({ final: "|" }, (() => this.setgLevel(3))), this._parser.registerEscHandler({ final: "}" }, (() => this.setgLevel(2))), this._parser.registerEscHandler({ final: "~" }, (() => this.setgLevel(1))), this._parser.registerEscHandler({ intermediates: "%", final: "@" }, (() => this.selectDefaultCharset())), this._parser.registerEscHandler({ intermediates: "%", final: "G" }, (() => this.selectDefaultCharset()));
        for (const e4 in o.CHARSETS) this._parser.registerEscHandler({ intermediates: "(", final: e4 }, (() => this.selectCharset("(" + e4))), this._parser.registerEscHandler({ intermediates: ")", final: e4 }, (() => this.selectCharset(")" + e4))), this._parser.registerEscHandler({ intermediates: "*", final: e4 }, (() => this.selectCharset("*" + e4))), this._parser.registerEscHandler({ intermediates: "+", final: e4 }, (() => this.selectCharset("+" + e4))), this._parser.registerEscHandler({ intermediates: "-", final: e4 }, (() => this.selectCharset("-" + e4))), this._parser.registerEscHandler({ intermediates: ".", final: e4 }, (() => this.selectCharset("." + e4))), this._parser.registerEscHandler({ intermediates: "/", final: e4 }, (() => this.selectCharset("/" + e4)));
        this._parser.registerEscHandler({ intermediates: "#", final: "8" }, (() => this.screenAlignmentPattern())), this._parser.setErrorHandler(((e4) => (this._logService.error("Parsing error: ", e4), e4))), this._parser.registerDcsHandler({ intermediates: "$", final: "q" }, new v.DcsHandler(((e4, t4) => this.requestStatusString(e4, t4))));
      }
      _preserveStack(e3, t3, s3, i3) {
        this._parseStack.paused = true, this._parseStack.cursorStartX = e3, this._parseStack.cursorStartY = t3, this._parseStack.decodedLength = s3, this._parseStack.position = i3;
      }
      _logSlowResolvingAsync(e3) {
        this._logService.logLevel <= _.LogLevelEnum.WARN && Promise.race([e3, new Promise(((e4, t3) => setTimeout((() => t3("#SLOW_TIMEOUT")), 5e3)))]).catch(((e4) => {
          if ("#SLOW_TIMEOUT" !== e4) throw e4;
          console.warn("async parser handler taking longer than 5000 ms");
        }));
      }
      _getCurrentLinkId() {
        return this._curAttrData.extended.urlId;
      }
      parse(e3, t3) {
        let s3, i3 = this._activeBuffer.x, r3 = this._activeBuffer.y, n3 = 0;
        const o2 = this._parseStack.paused;
        if (o2) {
          if (s3 = this._parser.parse(this._parseBuffer, this._parseStack.decodedLength, t3)) return this._logSlowResolvingAsync(s3), s3;
          i3 = this._parseStack.cursorStartX, r3 = this._parseStack.cursorStartY, this._parseStack.paused = false, e3.length > y && (n3 = this._parseStack.position + y);
        }
        if (this._logService.logLevel <= _.LogLevelEnum.DEBUG && this._logService.debug("parsing data " + ("string" == typeof e3 ? ` "${e3}"` : ` "${Array.prototype.map.call(e3, ((e4) => String.fromCharCode(e4))).join("")}"`)), this._logService.logLevel === _.LogLevelEnum.TRACE && this._logService.trace("parsing data (codes)", "string" == typeof e3 ? e3.split("").map(((e4) => e4.charCodeAt(0))) : e3), this._parseBuffer.length < e3.length && this._parseBuffer.length < y && (this._parseBuffer = new Uint32Array(Math.min(e3.length, y))), o2 || this._dirtyRowTracker.clearRange(), e3.length > y) for (let t4 = n3; t4 < e3.length; t4 += y) {
          const n4 = t4 + y < e3.length ? t4 + y : e3.length, o3 = "string" == typeof e3 ? this._stringDecoder.decode(e3.substring(t4, n4), this._parseBuffer) : this._utf8Decoder.decode(e3.subarray(t4, n4), this._parseBuffer);
          if (s3 = this._parser.parse(this._parseBuffer, o3)) return this._preserveStack(i3, r3, o3, t4), this._logSlowResolvingAsync(s3), s3;
        }
        else if (!o2) {
          const t4 = "string" == typeof e3 ? this._stringDecoder.decode(e3, this._parseBuffer) : this._utf8Decoder.decode(e3, this._parseBuffer);
          if (s3 = this._parser.parse(this._parseBuffer, t4)) return this._preserveStack(i3, r3, t4, 0), this._logSlowResolvingAsync(s3), s3;
        }
        this._activeBuffer.x === i3 && this._activeBuffer.y === r3 || this._onCursorMove.fire();
        const a2 = this._dirtyRowTracker.end + (this._bufferService.buffer.ybase - this._bufferService.buffer.ydisp), h2 = this._dirtyRowTracker.start + (this._bufferService.buffer.ybase - this._bufferService.buffer.ydisp);
        h2 < this._bufferService.rows && this._onRequestRefreshRows.fire({ start: Math.min(h2, this._bufferService.rows - 1), end: Math.min(a2, this._bufferService.rows - 1) });
      }
      print(e3, t3, s3) {
        let i3, r3;
        const n3 = this._charsetService.charset, o2 = this._optionsService.rawOptions.screenReaderMode, a2 = this._bufferService.cols, h2 = this._coreService.decPrivateModes.wraparound, d2 = this._coreService.modes.insertMode, f2 = this._curAttrData;
        let _2 = this._activeBuffer.lines.get(this._activeBuffer.ybase + this._activeBuffer.y);
        this._dirtyRowTracker.markDirty(this._activeBuffer.y), this._activeBuffer.x && s3 - t3 > 0 && 2 === _2.getWidth(this._activeBuffer.x - 1) && _2.setCellFromCodepoint(this._activeBuffer.x - 1, 0, 1, f2);
        let g2 = this._parser.precedingJoinState;
        for (let v2 = t3; v2 < s3; ++v2) {
          if (i3 = e3[v2], i3 < 127 && n3) {
            const e4 = n3[String.fromCharCode(i3)];
            e4 && (i3 = e4.charCodeAt(0));
          }
          const t4 = this._unicodeService.charProperties(i3, g2);
          r3 = p.UnicodeService.extractWidth(t4);
          const s4 = p.UnicodeService.extractShouldJoin(t4), m2 = s4 ? p.UnicodeService.extractWidth(g2) : 0;
          if (g2 = t4, o2 && this._onA11yChar.fire((0, c.stringFromCodePoint)(i3)), this._getCurrentLinkId() && this._oscLinkService.addLineToLink(this._getCurrentLinkId(), this._activeBuffer.ybase + this._activeBuffer.y), this._activeBuffer.x + r3 - m2 > a2) {
            if (h2) {
              const e4 = _2;
              let t5 = this._activeBuffer.x - m2;
              for (this._activeBuffer.x = m2, this._activeBuffer.y++, this._activeBuffer.y === this._activeBuffer.scrollBottom + 1 ? (this._activeBuffer.y--, this._bufferService.scroll(this._eraseAttrData(), true)) : (this._activeBuffer.y >= this._bufferService.rows && (this._activeBuffer.y = this._bufferService.rows - 1), this._activeBuffer.lines.get(this._activeBuffer.ybase + this._activeBuffer.y).isWrapped = true), _2 = this._activeBuffer.lines.get(this._activeBuffer.ybase + this._activeBuffer.y), m2 > 0 && _2 instanceof l.BufferLine && _2.copyCellsFrom(e4, t5, 0, m2, false); t5 < a2; ) e4.setCellFromCodepoint(t5++, 0, 1, f2);
            } else if (this._activeBuffer.x = a2 - 1, 2 === r3) continue;
          }
          if (s4 && this._activeBuffer.x) {
            const e4 = _2.getWidth(this._activeBuffer.x - 1) ? 1 : 2;
            _2.addCodepointToCell(this._activeBuffer.x - e4, i3, r3);
            for (let e5 = r3 - m2; --e5 >= 0; ) _2.setCellFromCodepoint(this._activeBuffer.x++, 0, 0, f2);
          } else if (d2 && (_2.insertCells(this._activeBuffer.x, r3 - m2, this._activeBuffer.getNullCell(f2)), 2 === _2.getWidth(a2 - 1) && _2.setCellFromCodepoint(a2 - 1, u.NULL_CELL_CODE, u.NULL_CELL_WIDTH, f2)), _2.setCellFromCodepoint(this._activeBuffer.x++, i3, r3, f2), r3 > 0) for (; --r3; ) _2.setCellFromCodepoint(this._activeBuffer.x++, 0, 0, f2);
        }
        this._parser.precedingJoinState = g2, this._activeBuffer.x < a2 && s3 - t3 > 0 && 0 === _2.getWidth(this._activeBuffer.x) && !_2.hasContent(this._activeBuffer.x) && _2.setCellFromCodepoint(this._activeBuffer.x, 0, 1, f2), this._dirtyRowTracker.markDirty(this._activeBuffer.y);
      }
      registerCsiHandler(e3, t3) {
        return "t" !== e3.final || e3.prefix || e3.intermediates ? this._parser.registerCsiHandler(e3, t3) : this._parser.registerCsiHandler(e3, ((e4) => !C(e4.params[0], this._optionsService.rawOptions.windowOptions) || t3(e4)));
      }
      registerDcsHandler(e3, t3) {
        return this._parser.registerDcsHandler(e3, new v.DcsHandler(t3));
      }
      registerEscHandler(e3, t3) {
        return this._parser.registerEscHandler(e3, t3);
      }
      registerOscHandler(e3, t3) {
        return this._parser.registerOscHandler(e3, new g.OscHandler(t3));
      }
      bell() {
        return this._onRequestBell.fire(), true;
      }
      lineFeed() {
        return this._dirtyRowTracker.markDirty(this._activeBuffer.y), this._optionsService.rawOptions.convertEol && (this._activeBuffer.x = 0), this._activeBuffer.y++, this._activeBuffer.y === this._activeBuffer.scrollBottom + 1 ? (this._activeBuffer.y--, this._bufferService.scroll(this._eraseAttrData())) : this._activeBuffer.y >= this._bufferService.rows ? this._activeBuffer.y = this._bufferService.rows - 1 : this._activeBuffer.lines.get(this._activeBuffer.ybase + this._activeBuffer.y).isWrapped = false, this._activeBuffer.x >= this._bufferService.cols && this._activeBuffer.x--, this._dirtyRowTracker.markDirty(this._activeBuffer.y), this._onLineFeed.fire(), true;
      }
      carriageReturn() {
        return this._activeBuffer.x = 0, true;
      }
      backspace() {
        var _a;
        if (!this._coreService.decPrivateModes.reverseWraparound) return this._restrictCursor(), this._activeBuffer.x > 0 && this._activeBuffer.x--, true;
        if (this._restrictCursor(this._bufferService.cols), this._activeBuffer.x > 0) this._activeBuffer.x--;
        else if (0 === this._activeBuffer.x && this._activeBuffer.y > this._activeBuffer.scrollTop && this._activeBuffer.y <= this._activeBuffer.scrollBottom && ((_a = this._activeBuffer.lines.get(this._activeBuffer.ybase + this._activeBuffer.y)) == null ? void 0 : _a.isWrapped)) {
          this._activeBuffer.lines.get(this._activeBuffer.ybase + this._activeBuffer.y).isWrapped = false, this._activeBuffer.y--, this._activeBuffer.x = this._bufferService.cols - 1;
          const e3 = this._activeBuffer.lines.get(this._activeBuffer.ybase + this._activeBuffer.y);
          e3.hasWidth(this._activeBuffer.x) && !e3.hasContent(this._activeBuffer.x) && this._activeBuffer.x--;
        }
        return this._restrictCursor(), true;
      }
      tab() {
        if (this._activeBuffer.x >= this._bufferService.cols) return true;
        const e3 = this._activeBuffer.x;
        return this._activeBuffer.x = this._activeBuffer.nextStop(), this._optionsService.rawOptions.screenReaderMode && this._onA11yTab.fire(this._activeBuffer.x - e3), true;
      }
      shiftOut() {
        return this._charsetService.setgLevel(1), true;
      }
      shiftIn() {
        return this._charsetService.setgLevel(0), true;
      }
      _restrictCursor(e3 = this._bufferService.cols - 1) {
        this._activeBuffer.x = Math.min(e3, Math.max(0, this._activeBuffer.x)), this._activeBuffer.y = this._coreService.decPrivateModes.origin ? Math.min(this._activeBuffer.scrollBottom, Math.max(this._activeBuffer.scrollTop, this._activeBuffer.y)) : Math.min(this._bufferService.rows - 1, Math.max(0, this._activeBuffer.y)), this._dirtyRowTracker.markDirty(this._activeBuffer.y);
      }
      _setCursor(e3, t3) {
        this._dirtyRowTracker.markDirty(this._activeBuffer.y), this._coreService.decPrivateModes.origin ? (this._activeBuffer.x = e3, this._activeBuffer.y = this._activeBuffer.scrollTop + t3) : (this._activeBuffer.x = e3, this._activeBuffer.y = t3), this._restrictCursor(), this._dirtyRowTracker.markDirty(this._activeBuffer.y);
      }
      _moveCursor(e3, t3) {
        this._restrictCursor(), this._setCursor(this._activeBuffer.x + e3, this._activeBuffer.y + t3);
      }
      cursorUp(e3) {
        const t3 = this._activeBuffer.y - this._activeBuffer.scrollTop;
        return t3 >= 0 ? this._moveCursor(0, -Math.min(t3, e3.params[0] || 1)) : this._moveCursor(0, -(e3.params[0] || 1)), true;
      }
      cursorDown(e3) {
        const t3 = this._activeBuffer.scrollBottom - this._activeBuffer.y;
        return t3 >= 0 ? this._moveCursor(0, Math.min(t3, e3.params[0] || 1)) : this._moveCursor(0, e3.params[0] || 1), true;
      }
      cursorForward(e3) {
        return this._moveCursor(e3.params[0] || 1, 0), true;
      }
      cursorBackward(e3) {
        return this._moveCursor(-(e3.params[0] || 1), 0), true;
      }
      cursorNextLine(e3) {
        return this.cursorDown(e3), this._activeBuffer.x = 0, true;
      }
      cursorPrecedingLine(e3) {
        return this.cursorUp(e3), this._activeBuffer.x = 0, true;
      }
      cursorCharAbsolute(e3) {
        return this._setCursor((e3.params[0] || 1) - 1, this._activeBuffer.y), true;
      }
      cursorPosition(e3) {
        return this._setCursor(e3.length >= 2 ? (e3.params[1] || 1) - 1 : 0, (e3.params[0] || 1) - 1), true;
      }
      charPosAbsolute(e3) {
        return this._setCursor((e3.params[0] || 1) - 1, this._activeBuffer.y), true;
      }
      hPositionRelative(e3) {
        return this._moveCursor(e3.params[0] || 1, 0), true;
      }
      linePosAbsolute(e3) {
        return this._setCursor(this._activeBuffer.x, (e3.params[0] || 1) - 1), true;
      }
      vPositionRelative(e3) {
        return this._moveCursor(0, e3.params[0] || 1), true;
      }
      hVPosition(e3) {
        return this.cursorPosition(e3), true;
      }
      tabClear(e3) {
        const t3 = e3.params[0];
        return 0 === t3 ? delete this._activeBuffer.tabs[this._activeBuffer.x] : 3 === t3 && (this._activeBuffer.tabs = {}), true;
      }
      cursorForwardTab(e3) {
        if (this._activeBuffer.x >= this._bufferService.cols) return true;
        let t3 = e3.params[0] || 1;
        for (; t3--; ) this._activeBuffer.x = this._activeBuffer.nextStop();
        return true;
      }
      cursorBackwardTab(e3) {
        if (this._activeBuffer.x >= this._bufferService.cols) return true;
        let t3 = e3.params[0] || 1;
        for (; t3--; ) this._activeBuffer.x = this._activeBuffer.prevStop();
        return true;
      }
      selectProtected(e3) {
        const t3 = e3.params[0];
        return 1 === t3 && (this._curAttrData.bg |= 536870912), 2 !== t3 && 0 !== t3 || (this._curAttrData.bg &= -536870913), true;
      }
      _eraseInBufferLine(e3, t3, s3, i3 = false, r3 = false) {
        const n3 = this._activeBuffer.lines.get(this._activeBuffer.ybase + e3);
        n3.replaceCells(t3, s3, this._activeBuffer.getNullCell(this._eraseAttrData()), r3), i3 && (n3.isWrapped = false);
      }
      _resetBufferLine(e3, t3 = false) {
        const s3 = this._activeBuffer.lines.get(this._activeBuffer.ybase + e3);
        s3 && (s3.fill(this._activeBuffer.getNullCell(this._eraseAttrData()), t3), this._bufferService.buffer.clearMarkers(this._activeBuffer.ybase + e3), s3.isWrapped = false);
      }
      eraseInDisplay(e3, t3 = false) {
        let s3;
        switch (this._restrictCursor(this._bufferService.cols), e3.params[0]) {
          case 0:
            for (s3 = this._activeBuffer.y, this._dirtyRowTracker.markDirty(s3), this._eraseInBufferLine(s3++, this._activeBuffer.x, this._bufferService.cols, 0 === this._activeBuffer.x, t3); s3 < this._bufferService.rows; s3++) this._resetBufferLine(s3, t3);
            this._dirtyRowTracker.markDirty(s3);
            break;
          case 1:
            for (s3 = this._activeBuffer.y, this._dirtyRowTracker.markDirty(s3), this._eraseInBufferLine(s3, 0, this._activeBuffer.x + 1, true, t3), this._activeBuffer.x + 1 >= this._bufferService.cols && (this._activeBuffer.lines.get(s3 + 1).isWrapped = false); s3--; ) this._resetBufferLine(s3, t3);
            this._dirtyRowTracker.markDirty(0);
            break;
          case 2:
            if (this._optionsService.rawOptions.scrollOnEraseInDisplay) {
              for (s3 = this._bufferService.rows, this._dirtyRowTracker.markRangeDirty(0, s3 - 1); s3--; ) {
                const e5 = this._activeBuffer.lines.get(this._activeBuffer.ybase + s3);
                if (e5 == null ? void 0 : e5.getTrimmedLength()) break;
              }
              for (; s3 >= 0; s3--) this._bufferService.scroll(this._eraseAttrData());
            } else {
              for (s3 = this._bufferService.rows, this._dirtyRowTracker.markDirty(s3 - 1); s3--; ) this._resetBufferLine(s3, t3);
              this._dirtyRowTracker.markDirty(0);
            }
            break;
          case 3:
            const e4 = this._activeBuffer.lines.length - this._bufferService.rows;
            e4 > 0 && (this._activeBuffer.lines.trimStart(e4), this._activeBuffer.ybase = Math.max(this._activeBuffer.ybase - e4, 0), this._activeBuffer.ydisp = Math.max(this._activeBuffer.ydisp - e4, 0), this._onScroll.fire(0));
        }
        return true;
      }
      eraseInLine(e3, t3 = false) {
        switch (this._restrictCursor(this._bufferService.cols), e3.params[0]) {
          case 0:
            this._eraseInBufferLine(this._activeBuffer.y, this._activeBuffer.x, this._bufferService.cols, 0 === this._activeBuffer.x, t3);
            break;
          case 1:
            this._eraseInBufferLine(this._activeBuffer.y, 0, this._activeBuffer.x + 1, false, t3);
            break;
          case 2:
            this._eraseInBufferLine(this._activeBuffer.y, 0, this._bufferService.cols, true, t3);
        }
        return this._dirtyRowTracker.markDirty(this._activeBuffer.y), true;
      }
      insertLines(e3) {
        this._restrictCursor();
        let t3 = e3.params[0] || 1;
        if (this._activeBuffer.y > this._activeBuffer.scrollBottom || this._activeBuffer.y < this._activeBuffer.scrollTop) return true;
        const s3 = this._activeBuffer.ybase + this._activeBuffer.y, i3 = this._bufferService.rows - 1 - this._activeBuffer.scrollBottom, r3 = this._bufferService.rows - 1 + this._activeBuffer.ybase - i3 + 1;
        for (; t3--; ) this._activeBuffer.lines.splice(r3 - 1, 1), this._activeBuffer.lines.splice(s3, 0, this._activeBuffer.getBlankLine(this._eraseAttrData()));
        return this._dirtyRowTracker.markRangeDirty(this._activeBuffer.y, this._activeBuffer.scrollBottom), this._activeBuffer.x = 0, true;
      }
      deleteLines(e3) {
        this._restrictCursor();
        let t3 = e3.params[0] || 1;
        if (this._activeBuffer.y > this._activeBuffer.scrollBottom || this._activeBuffer.y < this._activeBuffer.scrollTop) return true;
        const s3 = this._activeBuffer.ybase + this._activeBuffer.y;
        let i3;
        for (i3 = this._bufferService.rows - 1 - this._activeBuffer.scrollBottom, i3 = this._bufferService.rows - 1 + this._activeBuffer.ybase - i3; t3--; ) this._activeBuffer.lines.splice(s3, 1), this._activeBuffer.lines.splice(i3, 0, this._activeBuffer.getBlankLine(this._eraseAttrData()));
        return this._dirtyRowTracker.markRangeDirty(this._activeBuffer.y, this._activeBuffer.scrollBottom), this._activeBuffer.x = 0, true;
      }
      insertChars(e3) {
        this._restrictCursor();
        const t3 = this._activeBuffer.lines.get(this._activeBuffer.ybase + this._activeBuffer.y);
        return t3 && (t3.insertCells(this._activeBuffer.x, e3.params[0] || 1, this._activeBuffer.getNullCell(this._eraseAttrData())), this._dirtyRowTracker.markDirty(this._activeBuffer.y)), true;
      }
      deleteChars(e3) {
        this._restrictCursor();
        const t3 = this._activeBuffer.lines.get(this._activeBuffer.ybase + this._activeBuffer.y);
        return t3 && (t3.deleteCells(this._activeBuffer.x, e3.params[0] || 1, this._activeBuffer.getNullCell(this._eraseAttrData())), this._dirtyRowTracker.markDirty(this._activeBuffer.y)), true;
      }
      scrollUp(e3) {
        let t3 = e3.params[0] || 1;
        for (; t3--; ) this._activeBuffer.lines.splice(this._activeBuffer.ybase + this._activeBuffer.scrollTop, 1), this._activeBuffer.lines.splice(this._activeBuffer.ybase + this._activeBuffer.scrollBottom, 0, this._activeBuffer.getBlankLine(this._eraseAttrData()));
        return this._dirtyRowTracker.markRangeDirty(this._activeBuffer.scrollTop, this._activeBuffer.scrollBottom), true;
      }
      scrollDown(e3) {
        let t3 = e3.params[0] || 1;
        for (; t3--; ) this._activeBuffer.lines.splice(this._activeBuffer.ybase + this._activeBuffer.scrollBottom, 1), this._activeBuffer.lines.splice(this._activeBuffer.ybase + this._activeBuffer.scrollTop, 0, this._activeBuffer.getBlankLine(l.DEFAULT_ATTR_DATA));
        return this._dirtyRowTracker.markRangeDirty(this._activeBuffer.scrollTop, this._activeBuffer.scrollBottom), true;
      }
      scrollLeft(e3) {
        if (this._activeBuffer.y > this._activeBuffer.scrollBottom || this._activeBuffer.y < this._activeBuffer.scrollTop) return true;
        const t3 = e3.params[0] || 1;
        for (let e4 = this._activeBuffer.scrollTop; e4 <= this._activeBuffer.scrollBottom; ++e4) {
          const s3 = this._activeBuffer.lines.get(this._activeBuffer.ybase + e4);
          s3.deleteCells(0, t3, this._activeBuffer.getNullCell(this._eraseAttrData())), s3.isWrapped = false;
        }
        return this._dirtyRowTracker.markRangeDirty(this._activeBuffer.scrollTop, this._activeBuffer.scrollBottom), true;
      }
      scrollRight(e3) {
        if (this._activeBuffer.y > this._activeBuffer.scrollBottom || this._activeBuffer.y < this._activeBuffer.scrollTop) return true;
        const t3 = e3.params[0] || 1;
        for (let e4 = this._activeBuffer.scrollTop; e4 <= this._activeBuffer.scrollBottom; ++e4) {
          const s3 = this._activeBuffer.lines.get(this._activeBuffer.ybase + e4);
          s3.insertCells(0, t3, this._activeBuffer.getNullCell(this._eraseAttrData())), s3.isWrapped = false;
        }
        return this._dirtyRowTracker.markRangeDirty(this._activeBuffer.scrollTop, this._activeBuffer.scrollBottom), true;
      }
      insertColumns(e3) {
        if (this._activeBuffer.y > this._activeBuffer.scrollBottom || this._activeBuffer.y < this._activeBuffer.scrollTop) return true;
        const t3 = e3.params[0] || 1;
        for (let e4 = this._activeBuffer.scrollTop; e4 <= this._activeBuffer.scrollBottom; ++e4) {
          const s3 = this._activeBuffer.lines.get(this._activeBuffer.ybase + e4);
          s3.insertCells(this._activeBuffer.x, t3, this._activeBuffer.getNullCell(this._eraseAttrData())), s3.isWrapped = false;
        }
        return this._dirtyRowTracker.markRangeDirty(this._activeBuffer.scrollTop, this._activeBuffer.scrollBottom), true;
      }
      deleteColumns(e3) {
        if (this._activeBuffer.y > this._activeBuffer.scrollBottom || this._activeBuffer.y < this._activeBuffer.scrollTop) return true;
        const t3 = e3.params[0] || 1;
        for (let e4 = this._activeBuffer.scrollTop; e4 <= this._activeBuffer.scrollBottom; ++e4) {
          const s3 = this._activeBuffer.lines.get(this._activeBuffer.ybase + e4);
          s3.deleteCells(this._activeBuffer.x, t3, this._activeBuffer.getNullCell(this._eraseAttrData())), s3.isWrapped = false;
        }
        return this._dirtyRowTracker.markRangeDirty(this._activeBuffer.scrollTop, this._activeBuffer.scrollBottom), true;
      }
      eraseChars(e3) {
        this._restrictCursor();
        const t3 = this._activeBuffer.lines.get(this._activeBuffer.ybase + this._activeBuffer.y);
        return t3 && (t3.replaceCells(this._activeBuffer.x, this._activeBuffer.x + (e3.params[0] || 1), this._activeBuffer.getNullCell(this._eraseAttrData())), this._dirtyRowTracker.markDirty(this._activeBuffer.y)), true;
      }
      repeatPrecedingCharacter(e3) {
        const t3 = this._parser.precedingJoinState;
        if (!t3) return true;
        const s3 = e3.params[0] || 1, i3 = p.UnicodeService.extractWidth(t3), r3 = this._activeBuffer.x - i3, n3 = this._activeBuffer.lines.get(this._activeBuffer.ybase + this._activeBuffer.y).getString(r3), o2 = new Uint32Array(n3.length * s3);
        let a2 = 0;
        for (let e4 = 0; e4 < n3.length; ) {
          const t4 = n3.codePointAt(e4) || 0;
          o2[a2++] = t4, e4 += t4 > 65535 ? 2 : 1;
        }
        let h2 = a2;
        for (let e4 = 1; e4 < s3; ++e4) o2.copyWithin(h2, 0, a2), h2 += a2;
        return this.print(o2, 0, h2), true;
      }
      sendDeviceAttributesPrimary(e3) {
        return e3.params[0] > 0 || (this._is("xterm") || this._is("rxvt-unicode") || this._is("screen") ? this._coreService.triggerDataEvent(n2.C0.ESC + "[?1;2c") : this._is("linux") && this._coreService.triggerDataEvent(n2.C0.ESC + "[?6c")), true;
      }
      sendDeviceAttributesSecondary(e3) {
        return e3.params[0] > 0 || (this._is("xterm") ? this._coreService.triggerDataEvent(n2.C0.ESC + "[>0;276;0c") : this._is("rxvt-unicode") ? this._coreService.triggerDataEvent(n2.C0.ESC + "[>85;95;0c") : this._is("linux") ? this._coreService.triggerDataEvent(e3.params[0] + "c") : this._is("screen") && this._coreService.triggerDataEvent(n2.C0.ESC + "[>83;40003;0c")), true;
      }
      _is(e3) {
        return 0 === (this._optionsService.rawOptions.termName + "").indexOf(e3);
      }
      setMode(e3) {
        for (let t3 = 0; t3 < e3.length; t3++) switch (e3.params[t3]) {
          case 4:
            this._coreService.modes.insertMode = true;
            break;
          case 20:
            this._optionsService.options.convertEol = true;
        }
        return true;
      }
      setModePrivate(e3) {
        for (let t3 = 0; t3 < e3.length; t3++) switch (e3.params[t3]) {
          case 1:
            this._coreService.decPrivateModes.applicationCursorKeys = true;
            break;
          case 2:
            this._charsetService.setgCharset(0, o.DEFAULT_CHARSET), this._charsetService.setgCharset(1, o.DEFAULT_CHARSET), this._charsetService.setgCharset(2, o.DEFAULT_CHARSET), this._charsetService.setgCharset(3, o.DEFAULT_CHARSET);
            break;
          case 3:
            this._optionsService.rawOptions.windowOptions.setWinLines && (this._bufferService.resize(132, this._bufferService.rows), this._onRequestReset.fire());
            break;
          case 6:
            this._coreService.decPrivateModes.origin = true, this._setCursor(0, 0);
            break;
          case 7:
            this._coreService.decPrivateModes.wraparound = true;
            break;
          case 12:
            this._optionsService.options.cursorBlink = true;
            break;
          case 45:
            this._coreService.decPrivateModes.reverseWraparound = true;
            break;
          case 66:
            this._logService.debug("Serial port requested application keypad."), this._coreService.decPrivateModes.applicationKeypad = true, this._onRequestSyncScrollBar.fire();
            break;
          case 9:
            this._coreMouseService.activeProtocol = "X10";
            break;
          case 1e3:
            this._coreMouseService.activeProtocol = "VT200";
            break;
          case 1002:
            this._coreMouseService.activeProtocol = "DRAG";
            break;
          case 1003:
            this._coreMouseService.activeProtocol = "ANY";
            break;
          case 1004:
            this._coreService.decPrivateModes.sendFocus = true, this._onRequestSendFocus.fire();
            break;
          case 1005:
            this._logService.debug("DECSET 1005 not supported (see #2507)");
            break;
          case 1006:
            this._coreMouseService.activeEncoding = "SGR";
            break;
          case 1015:
            this._logService.debug("DECSET 1015 not supported (see #2507)");
            break;
          case 1016:
            this._coreMouseService.activeEncoding = "SGR_PIXELS";
            break;
          case 25:
            this._coreService.isCursorHidden = false;
            break;
          case 1048:
            this.saveCursor();
            break;
          case 1049:
            this.saveCursor();
          case 47:
          case 1047:
            this._bufferService.buffers.activateAltBuffer(this._eraseAttrData()), this._coreService.isCursorInitialized = true, this._onRequestRefreshRows.fire(void 0), this._onRequestSyncScrollBar.fire();
            break;
          case 2004:
            this._coreService.decPrivateModes.bracketedPasteMode = true;
            break;
          case 2026:
            this._coreService.decPrivateModes.synchronizedOutput = true;
        }
        return true;
      }
      resetMode(e3) {
        for (let t3 = 0; t3 < e3.length; t3++) switch (e3.params[t3]) {
          case 4:
            this._coreService.modes.insertMode = false;
            break;
          case 20:
            this._optionsService.options.convertEol = false;
        }
        return true;
      }
      resetModePrivate(e3) {
        for (let t3 = 0; t3 < e3.length; t3++) switch (e3.params[t3]) {
          case 1:
            this._coreService.decPrivateModes.applicationCursorKeys = false;
            break;
          case 3:
            this._optionsService.rawOptions.windowOptions.setWinLines && (this._bufferService.resize(80, this._bufferService.rows), this._onRequestReset.fire());
            break;
          case 6:
            this._coreService.decPrivateModes.origin = false, this._setCursor(0, 0);
            break;
          case 7:
            this._coreService.decPrivateModes.wraparound = false;
            break;
          case 12:
            this._optionsService.options.cursorBlink = false;
            break;
          case 45:
            this._coreService.decPrivateModes.reverseWraparound = false;
            break;
          case 66:
            this._logService.debug("Switching back to normal keypad."), this._coreService.decPrivateModes.applicationKeypad = false, this._onRequestSyncScrollBar.fire();
            break;
          case 9:
          case 1e3:
          case 1002:
          case 1003:
            this._coreMouseService.activeProtocol = "NONE";
            break;
          case 1004:
            this._coreService.decPrivateModes.sendFocus = false;
            break;
          case 1005:
            this._logService.debug("DECRST 1005 not supported (see #2507)");
            break;
          case 1006:
          case 1016:
            this._coreMouseService.activeEncoding = "DEFAULT";
            break;
          case 1015:
            this._logService.debug("DECRST 1015 not supported (see #2507)");
            break;
          case 25:
            this._coreService.isCursorHidden = true;
            break;
          case 1048:
            this.restoreCursor();
            break;
          case 1049:
          case 47:
          case 1047:
            this._bufferService.buffers.activateNormalBuffer(), 1049 === e3.params[t3] && this.restoreCursor(), this._coreService.isCursorInitialized = true, this._onRequestRefreshRows.fire(void 0), this._onRequestSyncScrollBar.fire();
            break;
          case 2004:
            this._coreService.decPrivateModes.bracketedPasteMode = false;
            break;
          case 2026:
            this._coreService.decPrivateModes.synchronizedOutput = false, this._onRequestRefreshRows.fire(void 0);
        }
        return true;
      }
      requestMode(e3, t3) {
        const s3 = this._coreService.decPrivateModes, { activeProtocol: i3, activeEncoding: r3 } = this._coreMouseService, o2 = this._coreService, { buffers: a2, cols: h2 } = this._bufferService, { active: c2, alt: l2 } = a2, u2 = this._optionsService.rawOptions, d2 = (e4) => e4 ? 1 : 2, f2 = e3.params[0];
        return _2 = f2, p2 = t3 ? 2 === f2 ? 4 : 4 === f2 ? d2(o2.modes.insertMode) : 12 === f2 ? 3 : 20 === f2 ? d2(u2.convertEol) : 0 : 1 === f2 ? d2(s3.applicationCursorKeys) : 3 === f2 ? u2.windowOptions.setWinLines ? 80 === h2 ? 2 : 132 === h2 ? 1 : 0 : 0 : 6 === f2 ? d2(s3.origin) : 7 === f2 ? d2(s3.wraparound) : 8 === f2 ? 3 : 9 === f2 ? d2("X10" === i3) : 12 === f2 ? d2(u2.cursorBlink) : 25 === f2 ? d2(!o2.isCursorHidden) : 45 === f2 ? d2(s3.reverseWraparound) : 66 === f2 ? d2(s3.applicationKeypad) : 67 === f2 ? 4 : 1e3 === f2 ? d2("VT200" === i3) : 1002 === f2 ? d2("DRAG" === i3) : 1003 === f2 ? d2("ANY" === i3) : 1004 === f2 ? d2(s3.sendFocus) : 1005 === f2 ? 4 : 1006 === f2 ? d2("SGR" === r3) : 1015 === f2 ? 4 : 1016 === f2 ? d2("SGR_PIXELS" === r3) : 1048 === f2 ? 1 : 47 === f2 || 1047 === f2 || 1049 === f2 ? d2(c2 === l2) : 2004 === f2 ? d2(s3.bracketedPasteMode) : 2026 === f2 ? d2(s3.synchronizedOutput) : 0, o2.triggerDataEvent(`${n2.C0.ESC}[${t3 ? "" : "?"}${_2};${p2}$y`), true;
        var _2, p2;
      }
      _updateAttrColor(e3, t3, s3, i3, r3) {
        return 2 === t3 ? (e3 |= 50331648, e3 &= -16777216, e3 |= f.AttributeData.fromColorRGB([s3, i3, r3])) : 5 === t3 && (e3 &= -50331904, e3 |= 33554432 | 255 & s3), e3;
      }
      _extractColor(e3, t3, s3) {
        const i3 = [0, 0, -1, 0, 0, 0];
        let r3 = 0, n3 = 0;
        do {
          if (i3[n3 + r3] = e3.params[t3 + n3], e3.hasSubParams(t3 + n3)) {
            const s4 = e3.getSubParams(t3 + n3);
            let o2 = 0;
            do {
              5 === i3[1] && (r3 = 1), i3[n3 + o2 + 1 + r3] = s4[o2];
            } while (++o2 < s4.length && o2 + n3 + 1 + r3 < i3.length);
            break;
          }
          if (5 === i3[1] && n3 + r3 >= 2 || 2 === i3[1] && n3 + r3 >= 5) break;
          i3[1] && (r3 = 1);
        } while (++n3 + t3 < e3.length && n3 + r3 < i3.length);
        for (let e4 = 2; e4 < i3.length; ++e4) -1 === i3[e4] && (i3[e4] = 0);
        switch (i3[0]) {
          case 38:
            s3.fg = this._updateAttrColor(s3.fg, i3[1], i3[3], i3[4], i3[5]);
            break;
          case 48:
            s3.bg = this._updateAttrColor(s3.bg, i3[1], i3[3], i3[4], i3[5]);
            break;
          case 58:
            s3.extended = s3.extended.clone(), s3.extended.underlineColor = this._updateAttrColor(s3.extended.underlineColor, i3[1], i3[3], i3[4], i3[5]);
        }
        return n3;
      }
      _processUnderline(e3, t3) {
        t3.extended = t3.extended.clone(), (!~e3 || e3 > 5) && (e3 = 1), t3.extended.underlineStyle = e3, t3.fg |= 268435456, 0 === e3 && (t3.fg &= -268435457), t3.updateExtended();
      }
      _processSGR0(e3) {
        e3.fg = l.DEFAULT_ATTR_DATA.fg, e3.bg = l.DEFAULT_ATTR_DATA.bg, e3.extended = e3.extended.clone(), e3.extended.underlineStyle = 0, e3.extended.underlineColor &= -67108864, e3.updateExtended();
      }
      charAttributes(e3) {
        if (1 === e3.length && 0 === e3.params[0]) return this._processSGR0(this._curAttrData), true;
        const t3 = e3.length;
        let s3;
        const i3 = this._curAttrData;
        for (let r3 = 0; r3 < t3; r3++) s3 = e3.params[r3], s3 >= 30 && s3 <= 37 ? (i3.fg &= -50331904, i3.fg |= 16777216 | s3 - 30) : s3 >= 40 && s3 <= 47 ? (i3.bg &= -50331904, i3.bg |= 16777216 | s3 - 40) : s3 >= 90 && s3 <= 97 ? (i3.fg &= -50331904, i3.fg |= 16777224 | s3 - 90) : s3 >= 100 && s3 <= 107 ? (i3.bg &= -50331904, i3.bg |= 16777224 | s3 - 100) : 0 === s3 ? this._processSGR0(i3) : 1 === s3 ? i3.fg |= 134217728 : 3 === s3 ? i3.bg |= 67108864 : 4 === s3 ? (i3.fg |= 268435456, this._processUnderline(e3.hasSubParams(r3) ? e3.getSubParams(r3)[0] : 1, i3)) : 5 === s3 ? i3.fg |= 536870912 : 7 === s3 ? i3.fg |= 67108864 : 8 === s3 ? i3.fg |= 1073741824 : 9 === s3 ? i3.fg |= 2147483648 : 2 === s3 ? i3.bg |= 134217728 : 21 === s3 ? this._processUnderline(2, i3) : 22 === s3 ? (i3.fg &= -134217729, i3.bg &= -134217729) : 23 === s3 ? i3.bg &= -67108865 : 24 === s3 ? (i3.fg &= -268435457, this._processUnderline(0, i3)) : 25 === s3 ? i3.fg &= -536870913 : 27 === s3 ? i3.fg &= -67108865 : 28 === s3 ? i3.fg &= -1073741825 : 29 === s3 ? i3.fg &= 2147483647 : 39 === s3 ? (i3.fg &= -67108864, i3.fg |= 16777215 & l.DEFAULT_ATTR_DATA.fg) : 49 === s3 ? (i3.bg &= -67108864, i3.bg |= 16777215 & l.DEFAULT_ATTR_DATA.bg) : 38 === s3 || 48 === s3 || 58 === s3 ? r3 += this._extractColor(e3, r3, i3) : 53 === s3 ? i3.bg |= 1073741824 : 55 === s3 ? i3.bg &= -1073741825 : 59 === s3 ? (i3.extended = i3.extended.clone(), i3.extended.underlineColor = -1, i3.updateExtended()) : 100 === s3 ? (i3.fg &= -67108864, i3.fg |= 16777215 & l.DEFAULT_ATTR_DATA.fg, i3.bg &= -67108864, i3.bg |= 16777215 & l.DEFAULT_ATTR_DATA.bg) : this._logService.debug("Unknown SGR attribute: %d.", s3);
        return true;
      }
      deviceStatus(e3) {
        switch (e3.params[0]) {
          case 5:
            this._coreService.triggerDataEvent(`${n2.C0.ESC}[0n`);
            break;
          case 6:
            const e4 = this._activeBuffer.y + 1, t3 = this._activeBuffer.x + 1;
            this._coreService.triggerDataEvent(`${n2.C0.ESC}[${e4};${t3}R`);
        }
        return true;
      }
      deviceStatusPrivate(e3) {
        if (6 === e3.params[0]) {
          const e4 = this._activeBuffer.y + 1, t3 = this._activeBuffer.x + 1;
          this._coreService.triggerDataEvent(`${n2.C0.ESC}[?${e4};${t3}R`);
        }
        return true;
      }
      softReset(e3) {
        return this._coreService.isCursorHidden = false, this._onRequestSyncScrollBar.fire(), this._activeBuffer.scrollTop = 0, this._activeBuffer.scrollBottom = this._bufferService.rows - 1, this._curAttrData = l.DEFAULT_ATTR_DATA.clone(), this._coreService.reset(), this._charsetService.reset(), this._activeBuffer.savedX = 0, this._activeBuffer.savedY = this._activeBuffer.ybase, this._activeBuffer.savedCurAttrData.fg = this._curAttrData.fg, this._activeBuffer.savedCurAttrData.bg = this._curAttrData.bg, this._activeBuffer.savedCharset = this._charsetService.charset, this._coreService.decPrivateModes.origin = false, true;
      }
      setCursorStyle(e3) {
        const t3 = 0 === e3.length ? 1 : e3.params[0];
        if (0 === t3) this._coreService.decPrivateModes.cursorStyle = void 0, this._coreService.decPrivateModes.cursorBlink = void 0;
        else {
          switch (t3) {
            case 1:
            case 2:
              this._coreService.decPrivateModes.cursorStyle = "block";
              break;
            case 3:
            case 4:
              this._coreService.decPrivateModes.cursorStyle = "underline";
              break;
            case 5:
            case 6:
              this._coreService.decPrivateModes.cursorStyle = "bar";
          }
          const e4 = t3 % 2 == 1;
          this._coreService.decPrivateModes.cursorBlink = e4;
        }
        return true;
      }
      setScrollRegion(e3) {
        const t3 = e3.params[0] || 1;
        let s3;
        return (e3.length < 2 || (s3 = e3.params[1]) > this._bufferService.rows || 0 === s3) && (s3 = this._bufferService.rows), s3 > t3 && (this._activeBuffer.scrollTop = t3 - 1, this._activeBuffer.scrollBottom = s3 - 1, this._setCursor(0, 0)), true;
      }
      windowOptions(e3) {
        if (!C(e3.params[0], this._optionsService.rawOptions.windowOptions)) return true;
        const t3 = e3.length > 1 ? e3.params[1] : 0;
        switch (e3.params[0]) {
          case 14:
            2 !== t3 && this._onRequestWindowsOptionsReport.fire(w.GET_WIN_SIZE_PIXELS);
            break;
          case 16:
            this._onRequestWindowsOptionsReport.fire(w.GET_CELL_SIZE_PIXELS);
            break;
          case 18:
            this._bufferService && this._coreService.triggerDataEvent(`${n2.C0.ESC}[8;${this._bufferService.rows};${this._bufferService.cols}t`);
            break;
          case 22:
            0 !== t3 && 2 !== t3 || (this._windowTitleStack.push(this._windowTitle), this._windowTitleStack.length > 10 && this._windowTitleStack.shift()), 0 !== t3 && 1 !== t3 || (this._iconNameStack.push(this._iconName), this._iconNameStack.length > 10 && this._iconNameStack.shift());
            break;
          case 23:
            0 !== t3 && 2 !== t3 || this._windowTitleStack.length && this.setTitle(this._windowTitleStack.pop()), 0 !== t3 && 1 !== t3 || this._iconNameStack.length && this.setIconName(this._iconNameStack.pop());
        }
        return true;
      }
      saveCursor(e3) {
        return this._activeBuffer.savedX = this._activeBuffer.x, this._activeBuffer.savedY = this._activeBuffer.ybase + this._activeBuffer.y, this._activeBuffer.savedCurAttrData.fg = this._curAttrData.fg, this._activeBuffer.savedCurAttrData.bg = this._curAttrData.bg, this._activeBuffer.savedCharset = this._charsetService.charset, true;
      }
      restoreCursor(e3) {
        return this._activeBuffer.x = this._activeBuffer.savedX || 0, this._activeBuffer.y = Math.max(this._activeBuffer.savedY - this._activeBuffer.ybase, 0), this._curAttrData.fg = this._activeBuffer.savedCurAttrData.fg, this._curAttrData.bg = this._activeBuffer.savedCurAttrData.bg, this._charsetService.charset = this._savedCharset, this._activeBuffer.savedCharset && (this._charsetService.charset = this._activeBuffer.savedCharset), this._restrictCursor(), true;
      }
      setTitle(e3) {
        return this._windowTitle = e3, this._onTitleChange.fire(e3), true;
      }
      setIconName(e3) {
        return this._iconName = e3, true;
      }
      setOrReportIndexedColor(e3) {
        const t3 = [], s3 = e3.split(";");
        for (; s3.length > 1; ) {
          const e4 = s3.shift(), i3 = s3.shift();
          if (/^\d+$/.exec(e4)) {
            const s4 = parseInt(e4);
            if (k(s4)) if ("?" === i3) t3.push({ type: 0, index: s4 });
            else {
              const e5 = (0, m.parseColor)(i3);
              e5 && t3.push({ type: 1, index: s4, color: e5 });
            }
          }
        }
        return t3.length && this._onColor.fire(t3), true;
      }
      setHyperlink(e3) {
        const t3 = e3.indexOf(";");
        if (-1 === t3) return true;
        const s3 = e3.slice(0, t3).trim(), i3 = e3.slice(t3 + 1);
        return i3 ? this._createHyperlink(s3, i3) : !s3.trim() && this._finishHyperlink();
      }
      _createHyperlink(e3, t3) {
        this._getCurrentLinkId() && this._finishHyperlink();
        const s3 = e3.split(":");
        let i3;
        const r3 = s3.findIndex(((e4) => e4.startsWith("id=")));
        return -1 !== r3 && (i3 = s3[r3].slice(3) || void 0), this._curAttrData.extended = this._curAttrData.extended.clone(), this._curAttrData.extended.urlId = this._oscLinkService.registerLink({ id: i3, uri: t3 }), this._curAttrData.updateExtended(), true;
      }
      _finishHyperlink() {
        return this._curAttrData.extended = this._curAttrData.extended.clone(), this._curAttrData.extended.urlId = 0, this._curAttrData.updateExtended(), true;
      }
      _setOrReportSpecialColor(e3, t3) {
        const s3 = e3.split(";");
        for (let e4 = 0; e4 < s3.length && !(t3 >= this._specialColors.length); ++e4, ++t3) if ("?" === s3[e4]) this._onColor.fire([{ type: 0, index: this._specialColors[t3] }]);
        else {
          const i3 = (0, m.parseColor)(s3[e4]);
          i3 && this._onColor.fire([{ type: 1, index: this._specialColors[t3], color: i3 }]);
        }
        return true;
      }
      setOrReportFgColor(e3) {
        return this._setOrReportSpecialColor(e3, 0);
      }
      setOrReportBgColor(e3) {
        return this._setOrReportSpecialColor(e3, 1);
      }
      setOrReportCursorColor(e3) {
        return this._setOrReportSpecialColor(e3, 2);
      }
      restoreIndexedColor(e3) {
        if (!e3) return this._onColor.fire([{ type: 2 }]), true;
        const t3 = [], s3 = e3.split(";");
        for (let e4 = 0; e4 < s3.length; ++e4) if (/^\d+$/.exec(s3[e4])) {
          const i3 = parseInt(s3[e4]);
          k(i3) && t3.push({ type: 2, index: i3 });
        }
        return t3.length && this._onColor.fire(t3), true;
      }
      restoreFgColor(e3) {
        return this._onColor.fire([{ type: 2, index: 256 }]), true;
      }
      restoreBgColor(e3) {
        return this._onColor.fire([{ type: 2, index: 257 }]), true;
      }
      restoreCursorColor(e3) {
        return this._onColor.fire([{ type: 2, index: 258 }]), true;
      }
      nextLine() {
        return this._activeBuffer.x = 0, this.index(), true;
      }
      keypadApplicationMode() {
        return this._logService.debug("Serial port requested application keypad."), this._coreService.decPrivateModes.applicationKeypad = true, this._onRequestSyncScrollBar.fire(), true;
      }
      keypadNumericMode() {
        return this._logService.debug("Switching back to normal keypad."), this._coreService.decPrivateModes.applicationKeypad = false, this._onRequestSyncScrollBar.fire(), true;
      }
      selectDefaultCharset() {
        return this._charsetService.setgLevel(0), this._charsetService.setgCharset(0, o.DEFAULT_CHARSET), true;
      }
      selectCharset(e3) {
        return 2 !== e3.length ? (this.selectDefaultCharset(), true) : ("/" === e3[0] || this._charsetService.setgCharset(S[e3[0]], o.CHARSETS[e3[1]] || o.DEFAULT_CHARSET), true);
      }
      index() {
        return this._restrictCursor(), this._activeBuffer.y++, this._activeBuffer.y === this._activeBuffer.scrollBottom + 1 ? (this._activeBuffer.y--, this._bufferService.scroll(this._eraseAttrData())) : this._activeBuffer.y >= this._bufferService.rows && (this._activeBuffer.y = this._bufferService.rows - 1), this._restrictCursor(), true;
      }
      tabSet() {
        return this._activeBuffer.tabs[this._activeBuffer.x] = true, true;
      }
      reverseIndex() {
        if (this._restrictCursor(), this._activeBuffer.y === this._activeBuffer.scrollTop) {
          const e3 = this._activeBuffer.scrollBottom - this._activeBuffer.scrollTop;
          this._activeBuffer.lines.shiftElements(this._activeBuffer.ybase + this._activeBuffer.y, e3, 1), this._activeBuffer.lines.set(this._activeBuffer.ybase + this._activeBuffer.y, this._activeBuffer.getBlankLine(this._eraseAttrData())), this._dirtyRowTracker.markRangeDirty(this._activeBuffer.scrollTop, this._activeBuffer.scrollBottom);
        } else this._activeBuffer.y--, this._restrictCursor();
        return true;
      }
      fullReset() {
        return this._parser.reset(), this._onRequestReset.fire(), true;
      }
      reset() {
        this._curAttrData = l.DEFAULT_ATTR_DATA.clone(), this._eraseAttrDataInternal = l.DEFAULT_ATTR_DATA.clone();
      }
      _eraseAttrData() {
        return this._eraseAttrDataInternal.bg &= -67108864, this._eraseAttrDataInternal.bg |= 67108863 & this._curAttrData.bg, this._eraseAttrDataInternal;
      }
      setgLevel(e3) {
        return this._charsetService.setgLevel(e3), true;
      }
      screenAlignmentPattern() {
        const e3 = new d.CellData();
        e3.content = 1 << 22 | "E".charCodeAt(0), e3.fg = this._curAttrData.fg, e3.bg = this._curAttrData.bg, this._setCursor(0, 0);
        for (let t3 = 0; t3 < this._bufferService.rows; ++t3) {
          const s3 = this._activeBuffer.ybase + this._activeBuffer.y + t3, i3 = this._activeBuffer.lines.get(s3);
          i3 && (i3.fill(e3), i3.isWrapped = false);
        }
        return this._dirtyRowTracker.markAllDirty(), this._setCursor(0, 0), true;
      }
      requestStatusString(e3, t3) {
        const s3 = this._bufferService.buffer, i3 = this._optionsService.rawOptions;
        return ((e4) => (this._coreService.triggerDataEvent(`${n2.C0.ESC}${e4}${n2.C0.ESC}\\`), true))('"q' === e3 ? `P1$r${this._curAttrData.isProtected() ? 1 : 0}"q` : '"p' === e3 ? 'P1$r61;1"p' : "r" === e3 ? `P1$r${s3.scrollTop + 1};${s3.scrollBottom + 1}r` : "m" === e3 ? "P1$r0m" : " q" === e3 ? `P1$r${{ block: 2, underline: 4, bar: 6 }[i3.cursorStyle] - (i3.cursorBlink ? 1 : 0)} q` : "P0$r");
      }
      markRangeDirty(e3, t3) {
        this._dirtyRowTracker.markRangeDirty(e3, t3);
      }
    }
    t2.InputHandler = A;
    let L = class {
      constructor(e3) {
        this._bufferService = e3, this.clearRange();
      }
      clearRange() {
        this.start = this._bufferService.buffer.y, this.end = this._bufferService.buffer.y;
      }
      markDirty(e3) {
        e3 < this.start ? this.start = e3 : e3 > this.end && (this.end = e3);
      }
      markRangeDirty(e3, t3) {
        e3 > t3 && (E = e3, e3 = t3, t3 = E), e3 < this.start && (this.start = e3), t3 > this.end && (this.end = t3);
      }
      markAllDirty() {
        this.markRangeDirty(0, this._bufferService.rows - 1);
      }
    };
    function k(e3) {
      return 0 <= e3 && e3 < 256;
    }
    L = i2([r2(0, _.IBufferService)], L);
  }, 701: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.isChromeOS = t2.isLinux = t2.isWindows = t2.isIphone = t2.isIpad = t2.isMac = t2.isSafari = t2.isLegacyEdge = t2.isFirefox = t2.isNode = void 0, t2.getSafariVersion = function() {
      if (!t2.isSafari) return 0;
      const e3 = s2.match(/Version\/(\d+)/);
      return null === e3 || e3.length < 2 ? 0 : parseInt(e3[1]);
    }, t2.isNode = "undefined" != typeof process && "title" in process;
    const s2 = t2.isNode ? "node" : navigator.userAgent, i2 = t2.isNode ? "node" : navigator.platform;
    t2.isFirefox = s2.includes("Firefox"), t2.isLegacyEdge = s2.includes("Edge"), t2.isSafari = /^((?!chrome|android).)*safari/i.test(s2), t2.isMac = ["Macintosh", "MacIntel", "MacPPC", "Mac68K"].includes(i2), t2.isIpad = "iPad" === i2, t2.isIphone = "iPhone" === i2, t2.isWindows = ["Windows", "Win16", "Win32", "WinCE"].includes(i2), t2.isLinux = i2.indexOf("Linux") >= 0, t2.isChromeOS = /\bCrOS\b/.test(s2);
  }, 6168: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.DebouncedIdleTask = t2.IdleTaskQueue = t2.PriorityTaskQueue = void 0;
    const i2 = s2(701);
    class r2 {
      constructor() {
        this._tasks = [], this._i = 0;
      }
      enqueue(e3) {
        this._tasks.push(e3), this._start();
      }
      flush() {
        for (; this._i < this._tasks.length; ) this._tasks[this._i]() || this._i++;
        this.clear();
      }
      clear() {
        this._idleCallback && (this._cancelCallback(this._idleCallback), this._idleCallback = void 0), this._i = 0, this._tasks.length = 0;
      }
      _start() {
        this._idleCallback || (this._idleCallback = this._requestCallback(this._process.bind(this)));
      }
      _process(e3) {
        this._idleCallback = void 0;
        let t3 = 0, s3 = 0, i3 = e3.timeRemaining(), r3 = 0;
        for (; this._i < this._tasks.length; ) {
          if (t3 = performance.now(), this._tasks[this._i]() || this._i++, t3 = Math.max(1, performance.now() - t3), s3 = Math.max(t3, s3), r3 = e3.timeRemaining(), 1.5 * s3 > r3) return i3 - t3 < -20 && console.warn(`task queue exceeded allotted deadline by ${Math.abs(Math.round(i3 - t3))}ms`), void this._start();
          i3 = r3;
        }
        this.clear();
      }
    }
    class n2 extends r2 {
      _requestCallback(e3) {
        return setTimeout((() => e3(this._createDeadline(16))));
      }
      _cancelCallback(e3) {
        clearTimeout(e3);
      }
      _createDeadline(e3) {
        const t3 = performance.now() + e3;
        return { timeRemaining: () => Math.max(0, t3 - performance.now()) };
      }
    }
    t2.PriorityTaskQueue = n2, t2.IdleTaskQueue = !i2.isNode && "requestIdleCallback" in window ? class extends r2 {
      _requestCallback(e3) {
        return requestIdleCallback(e3);
      }
      _cancelCallback(e3) {
        cancelIdleCallback(e3);
      }
    } : n2, t2.DebouncedIdleTask = class {
      constructor() {
        this._queue = new t2.IdleTaskQueue();
      }
      set(e3) {
        this._queue.clear(), this._queue.enqueue(e3);
      }
      flush() {
        this._queue.flush();
      }
    };
  }, 5882: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.updateWindowsModeWrappedState = function(e3) {
      const t3 = e3.buffer.lines.get(e3.buffer.ybase + e3.buffer.y - 1), s3 = t3 == null ? void 0 : t3.get(e3.cols - 1), r2 = e3.buffer.lines.get(e3.buffer.ybase + e3.buffer.y);
      r2 && s3 && (r2.isWrapped = s3[i2.CHAR_DATA_CODE_INDEX] !== i2.NULL_CELL_CODE && s3[i2.CHAR_DATA_CODE_INDEX] !== i2.WHITESPACE_CELL_CODE);
    };
    const i2 = s2(8938);
  }, 5451: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.ExtendedAttrs = t2.AttributeData = void 0;
    class s2 {
      constructor() {
        this.fg = 0, this.bg = 0, this.extended = new i2();
      }
      static toColorRGB(e3) {
        return [e3 >>> 16 & 255, e3 >>> 8 & 255, 255 & e3];
      }
      static fromColorRGB(e3) {
        return (255 & e3[0]) << 16 | (255 & e3[1]) << 8 | 255 & e3[2];
      }
      clone() {
        const e3 = new s2();
        return e3.fg = this.fg, e3.bg = this.bg, e3.extended = this.extended.clone(), e3;
      }
      isInverse() {
        return 67108864 & this.fg;
      }
      isBold() {
        return 134217728 & this.fg;
      }
      isUnderline() {
        return this.hasExtendedAttrs() && 0 !== this.extended.underlineStyle ? 1 : 268435456 & this.fg;
      }
      isBlink() {
        return 536870912 & this.fg;
      }
      isInvisible() {
        return 1073741824 & this.fg;
      }
      isItalic() {
        return 67108864 & this.bg;
      }
      isDim() {
        return 134217728 & this.bg;
      }
      isStrikethrough() {
        return 2147483648 & this.fg;
      }
      isProtected() {
        return 536870912 & this.bg;
      }
      isOverline() {
        return 1073741824 & this.bg;
      }
      getFgColorMode() {
        return 50331648 & this.fg;
      }
      getBgColorMode() {
        return 50331648 & this.bg;
      }
      isFgRGB() {
        return !(50331648 & ~this.fg);
      }
      isBgRGB() {
        return !(50331648 & ~this.bg);
      }
      isFgPalette() {
        return 16777216 == (50331648 & this.fg) || 33554432 == (50331648 & this.fg);
      }
      isBgPalette() {
        return 16777216 == (50331648 & this.bg) || 33554432 == (50331648 & this.bg);
      }
      isFgDefault() {
        return !(50331648 & this.fg);
      }
      isBgDefault() {
        return !(50331648 & this.bg);
      }
      isAttributeDefault() {
        return 0 === this.fg && 0 === this.bg;
      }
      getFgColor() {
        switch (50331648 & this.fg) {
          case 16777216:
          case 33554432:
            return 255 & this.fg;
          case 50331648:
            return 16777215 & this.fg;
          default:
            return -1;
        }
      }
      getBgColor() {
        switch (50331648 & this.bg) {
          case 16777216:
          case 33554432:
            return 255 & this.bg;
          case 50331648:
            return 16777215 & this.bg;
          default:
            return -1;
        }
      }
      hasExtendedAttrs() {
        return 268435456 & this.bg;
      }
      updateExtended() {
        this.extended.isEmpty() ? this.bg &= -268435457 : this.bg |= 268435456;
      }
      getUnderlineColor() {
        if (268435456 & this.bg && ~this.extended.underlineColor) switch (50331648 & this.extended.underlineColor) {
          case 16777216:
          case 33554432:
            return 255 & this.extended.underlineColor;
          case 50331648:
            return 16777215 & this.extended.underlineColor;
          default:
            return this.getFgColor();
        }
        return this.getFgColor();
      }
      getUnderlineColorMode() {
        return 268435456 & this.bg && ~this.extended.underlineColor ? 50331648 & this.extended.underlineColor : this.getFgColorMode();
      }
      isUnderlineColorRGB() {
        return 268435456 & this.bg && ~this.extended.underlineColor ? !(50331648 & ~this.extended.underlineColor) : this.isFgRGB();
      }
      isUnderlineColorPalette() {
        return 268435456 & this.bg && ~this.extended.underlineColor ? 16777216 == (50331648 & this.extended.underlineColor) || 33554432 == (50331648 & this.extended.underlineColor) : this.isFgPalette();
      }
      isUnderlineColorDefault() {
        return 268435456 & this.bg && ~this.extended.underlineColor ? !(50331648 & this.extended.underlineColor) : this.isFgDefault();
      }
      getUnderlineStyle() {
        return 268435456 & this.fg ? 268435456 & this.bg ? this.extended.underlineStyle : 1 : 0;
      }
      getUnderlineVariantOffset() {
        return this.extended.underlineVariantOffset;
      }
    }
    t2.AttributeData = s2;
    class i2 {
      get ext() {
        return this._urlId ? -469762049 & this._ext | this.underlineStyle << 26 : this._ext;
      }
      set ext(e3) {
        this._ext = e3;
      }
      get underlineStyle() {
        return this._urlId ? 5 : (469762048 & this._ext) >> 26;
      }
      set underlineStyle(e3) {
        this._ext &= -469762049, this._ext |= e3 << 26 & 469762048;
      }
      get underlineColor() {
        return 67108863 & this._ext;
      }
      set underlineColor(e3) {
        this._ext &= -67108864, this._ext |= 67108863 & e3;
      }
      get urlId() {
        return this._urlId;
      }
      set urlId(e3) {
        this._urlId = e3;
      }
      get underlineVariantOffset() {
        const e3 = (3758096384 & this._ext) >> 29;
        return e3 < 0 ? 4294967288 ^ e3 : e3;
      }
      set underlineVariantOffset(e3) {
        this._ext &= 536870911, this._ext |= e3 << 29 & 3758096384;
      }
      constructor(e3 = 0, t3 = 0) {
        this._ext = 0, this._urlId = 0, this._ext = e3, this._urlId = t3;
      }
      clone() {
        return new i2(this._ext, this._urlId);
      }
      isEmpty() {
        return 0 === this.underlineStyle && 0 === this._urlId;
      }
    }
    t2.ExtendedAttrs = i2;
  }, 1073: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.Buffer = t2.MAX_BUFFER_SIZE = void 0;
    const i2 = s2(5639), r2 = s2(6168), n2 = s2(5451), o = s2(6107), a = s2(732), h = s2(3055), c = s2(8938), l = s2(8158), u = s2(6760);
    t2.MAX_BUFFER_SIZE = 4294967295, t2.Buffer = class {
      constructor(e3, t3, s3) {
        this._hasScrollback = e3, this._optionsService = t3, this._bufferService = s3, this.ydisp = 0, this.ybase = 0, this.y = 0, this.x = 0, this.tabs = {}, this.savedY = 0, this.savedX = 0, this.savedCurAttrData = o.DEFAULT_ATTR_DATA.clone(), this.savedCharset = u.DEFAULT_CHARSET, this.markers = [], this._nullCell = h.CellData.fromCharData([0, c.NULL_CELL_CHAR, c.NULL_CELL_WIDTH, c.NULL_CELL_CODE]), this._whitespaceCell = h.CellData.fromCharData([0, c.WHITESPACE_CELL_CHAR, c.WHITESPACE_CELL_WIDTH, c.WHITESPACE_CELL_CODE]), this._isClearing = false, this._memoryCleanupQueue = new r2.IdleTaskQueue(), this._memoryCleanupPosition = 0, this._cols = this._bufferService.cols, this._rows = this._bufferService.rows, this.lines = new i2.CircularList(this._getCorrectBufferLength(this._rows)), this.scrollTop = 0, this.scrollBottom = this._rows - 1, this.setupTabStops();
      }
      getNullCell(e3) {
        return e3 ? (this._nullCell.fg = e3.fg, this._nullCell.bg = e3.bg, this._nullCell.extended = e3.extended) : (this._nullCell.fg = 0, this._nullCell.bg = 0, this._nullCell.extended = new n2.ExtendedAttrs()), this._nullCell;
      }
      getWhitespaceCell(e3) {
        return e3 ? (this._whitespaceCell.fg = e3.fg, this._whitespaceCell.bg = e3.bg, this._whitespaceCell.extended = e3.extended) : (this._whitespaceCell.fg = 0, this._whitespaceCell.bg = 0, this._whitespaceCell.extended = new n2.ExtendedAttrs()), this._whitespaceCell;
      }
      getBlankLine(e3, t3) {
        return new o.BufferLine(this._bufferService.cols, this.getNullCell(e3), t3);
      }
      get hasScrollback() {
        return this._hasScrollback && this.lines.maxLength > this._rows;
      }
      get isCursorInViewport() {
        const e3 = this.ybase + this.y - this.ydisp;
        return e3 >= 0 && e3 < this._rows;
      }
      _getCorrectBufferLength(e3) {
        if (!this._hasScrollback) return e3;
        const s3 = e3 + this._optionsService.rawOptions.scrollback;
        return s3 > t2.MAX_BUFFER_SIZE ? t2.MAX_BUFFER_SIZE : s3;
      }
      fillViewportRows(e3) {
        if (0 === this.lines.length) {
          void 0 === e3 && (e3 = o.DEFAULT_ATTR_DATA);
          let t3 = this._rows;
          for (; t3--; ) this.lines.push(this.getBlankLine(e3));
        }
      }
      clear() {
        this.ydisp = 0, this.ybase = 0, this.y = 0, this.x = 0, this.lines = new i2.CircularList(this._getCorrectBufferLength(this._rows)), this.scrollTop = 0, this.scrollBottom = this._rows - 1, this.setupTabStops();
      }
      resize(e3, t3) {
        const s3 = this.getNullCell(o.DEFAULT_ATTR_DATA);
        let i3 = 0;
        const r3 = this._getCorrectBufferLength(t3);
        if (r3 > this.lines.maxLength && (this.lines.maxLength = r3), this.lines.length > 0) {
          if (this._cols < e3) for (let t4 = 0; t4 < this.lines.length; t4++) i3 += +this.lines.get(t4).resize(e3, s3);
          let n3 = 0;
          if (this._rows < t3) for (let i4 = this._rows; i4 < t3; i4++) this.lines.length < t3 + this.ybase && (this._optionsService.rawOptions.windowsMode || void 0 !== this._optionsService.rawOptions.windowsPty.backend || void 0 !== this._optionsService.rawOptions.windowsPty.buildNumber ? this.lines.push(new o.BufferLine(e3, s3)) : this.ybase > 0 && this.lines.length <= this.ybase + this.y + n3 + 1 ? (this.ybase--, n3++, this.ydisp > 0 && this.ydisp--) : this.lines.push(new o.BufferLine(e3, s3)));
          else for (let e4 = this._rows; e4 > t3; e4--) this.lines.length > t3 + this.ybase && (this.lines.length > this.ybase + this.y + 1 ? this.lines.pop() : (this.ybase++, this.ydisp++));
          if (r3 < this.lines.maxLength) {
            const e4 = this.lines.length - r3;
            e4 > 0 && (this.lines.trimStart(e4), this.ybase = Math.max(this.ybase - e4, 0), this.ydisp = Math.max(this.ydisp - e4, 0), this.savedY = Math.max(this.savedY - e4, 0)), this.lines.maxLength = r3;
          }
          this.x = Math.min(this.x, e3 - 1), this.y = Math.min(this.y, t3 - 1), n3 && (this.y += n3), this.savedX = Math.min(this.savedX, e3 - 1), this.scrollTop = 0;
        }
        if (this.scrollBottom = t3 - 1, this._isReflowEnabled && (this._reflow(e3, t3), this._cols > e3)) for (let t4 = 0; t4 < this.lines.length; t4++) i3 += +this.lines.get(t4).resize(e3, s3);
        this._cols = e3, this._rows = t3, this._memoryCleanupQueue.clear(), i3 > 0.1 * this.lines.length && (this._memoryCleanupPosition = 0, this._memoryCleanupQueue.enqueue((() => this._batchedMemoryCleanup())));
      }
      _batchedMemoryCleanup() {
        let e3 = true;
        this._memoryCleanupPosition >= this.lines.length && (this._memoryCleanupPosition = 0, e3 = false);
        let t3 = 0;
        for (; this._memoryCleanupPosition < this.lines.length; ) if (t3 += this.lines.get(this._memoryCleanupPosition++).cleanupMemory(), t3 > 100) return true;
        return e3;
      }
      get _isReflowEnabled() {
        const e3 = this._optionsService.rawOptions.windowsPty;
        return e3 && e3.buildNumber ? this._hasScrollback && "conpty" === e3.backend && e3.buildNumber >= 21376 : this._hasScrollback && !this._optionsService.rawOptions.windowsMode;
      }
      _reflow(e3, t3) {
        this._cols !== e3 && (e3 > this._cols ? this._reflowLarger(e3, t3) : this._reflowSmaller(e3, t3));
      }
      _reflowLarger(e3, t3) {
        const s3 = this._optionsService.rawOptions.reflowCursorLine, i3 = (0, a.reflowLargerGetLinesToRemove)(this.lines, this._cols, e3, this.ybase + this.y, this.getNullCell(o.DEFAULT_ATTR_DATA), s3);
        if (i3.length > 0) {
          const s4 = (0, a.reflowLargerCreateNewLayout)(this.lines, i3);
          (0, a.reflowLargerApplyNewLayout)(this.lines, s4.layout), this._reflowLargerAdjustViewport(e3, t3, s4.countRemoved);
        }
      }
      _reflowLargerAdjustViewport(e3, t3, s3) {
        const i3 = this.getNullCell(o.DEFAULT_ATTR_DATA);
        let r3 = s3;
        for (; r3-- > 0; ) 0 === this.ybase ? (this.y > 0 && this.y--, this.lines.length < t3 && this.lines.push(new o.BufferLine(e3, i3))) : (this.ydisp === this.ybase && this.ydisp--, this.ybase--);
        this.savedY = Math.max(this.savedY - s3, 0);
      }
      _reflowSmaller(e3, t3) {
        const s3 = this._optionsService.rawOptions.reflowCursorLine, i3 = this.getNullCell(o.DEFAULT_ATTR_DATA), r3 = [];
        let n3 = 0;
        for (let h2 = this.lines.length - 1; h2 >= 0; h2--) {
          let c2 = this.lines.get(h2);
          if (!c2 || !c2.isWrapped && c2.getTrimmedLength() <= e3) continue;
          const l2 = [c2];
          for (; c2.isWrapped && h2 > 0; ) c2 = this.lines.get(--h2), l2.unshift(c2);
          if (!s3) {
            const e4 = this.ybase + this.y;
            if (e4 >= h2 && e4 < h2 + l2.length) continue;
          }
          const u2 = l2[l2.length - 1].getTrimmedLength(), d = (0, a.reflowSmallerGetNewLineLengths)(l2, this._cols, e3), f = d.length - l2.length;
          let _;
          _ = 0 === this.ybase && this.y !== this.lines.length - 1 ? Math.max(0, this.y - this.lines.maxLength + f) : Math.max(0, this.lines.length - this.lines.maxLength + f);
          const p = [];
          for (let e4 = 0; e4 < f; e4++) {
            const e5 = this.getBlankLine(o.DEFAULT_ATTR_DATA, true);
            p.push(e5);
          }
          p.length > 0 && (r3.push({ start: h2 + l2.length + n3, newLines: p }), n3 += p.length), l2.push(...p);
          let g = d.length - 1, v = d[g];
          0 === v && (g--, v = d[g]);
          let m = l2.length - f - 1, b = u2;
          for (; m >= 0; ) {
            const e4 = Math.min(b, v);
            if (void 0 === l2[g]) break;
            if (l2[g].copyCellsFrom(l2[m], b - e4, v - e4, e4, true), v -= e4, 0 === v && (g--, v = d[g]), b -= e4, 0 === b) {
              m--;
              const e5 = Math.max(m, 0);
              b = (0, a.getWrappedLineTrimmedLength)(l2, e5, this._cols);
            }
          }
          for (let t4 = 0; t4 < l2.length; t4++) d[t4] < e3 && l2[t4].setCell(d[t4], i3);
          let S = f - _;
          for (; S-- > 0; ) 0 === this.ybase ? this.y < t3 - 1 ? (this.y++, this.lines.pop()) : (this.ybase++, this.ydisp++) : this.ybase < Math.min(this.lines.maxLength, this.lines.length + n3) - t3 && (this.ybase === this.ydisp && this.ydisp++, this.ybase++);
          this.savedY = Math.min(this.savedY + f, this.ybase + t3 - 1);
        }
        if (r3.length > 0) {
          const e4 = [], t4 = [];
          for (let e5 = 0; e5 < this.lines.length; e5++) t4.push(this.lines.get(e5));
          const s4 = this.lines.length;
          let i4 = s4 - 1, o2 = 0, a2 = r3[o2];
          this.lines.length = Math.min(this.lines.maxLength, this.lines.length + n3);
          let h2 = 0;
          for (let c3 = Math.min(this.lines.maxLength - 1, s4 + n3 - 1); c3 >= 0; c3--) if (a2 && a2.start > i4 + h2) {
            for (let e5 = a2.newLines.length - 1; e5 >= 0; e5--) this.lines.set(c3--, a2.newLines[e5]);
            c3++, e4.push({ index: i4 + 1, amount: a2.newLines.length }), h2 += a2.newLines.length, a2 = r3[++o2];
          } else this.lines.set(c3, t4[i4--]);
          let c2 = 0;
          for (let t5 = e4.length - 1; t5 >= 0; t5--) e4[t5].index += c2, this.lines.onInsertEmitter.fire(e4[t5]), c2 += e4[t5].amount;
          const l2 = Math.max(0, s4 + n3 - this.lines.maxLength);
          l2 > 0 && this.lines.onTrimEmitter.fire(l2);
        }
      }
      translateBufferLineToString(e3, t3, s3 = 0, i3) {
        const r3 = this.lines.get(e3);
        return r3 ? r3.translateToString(t3, s3, i3) : "";
      }
      getWrappedRangeForLine(e3) {
        let t3 = e3, s3 = e3;
        for (; t3 > 0 && this.lines.get(t3).isWrapped; ) t3--;
        for (; s3 + 1 < this.lines.length && this.lines.get(s3 + 1).isWrapped; ) s3++;
        return { first: t3, last: s3 };
      }
      setupTabStops(e3) {
        for (null != e3 ? this.tabs[e3] || (e3 = this.prevStop(e3)) : (this.tabs = {}, e3 = 0); e3 < this._cols; e3 += this._optionsService.rawOptions.tabStopWidth) this.tabs[e3] = true;
      }
      prevStop(e3) {
        for (null == e3 && (e3 = this.x); !this.tabs[--e3] && e3 > 0; ) ;
        return e3 >= this._cols ? this._cols - 1 : e3 < 0 ? 0 : e3;
      }
      nextStop(e3) {
        for (null == e3 && (e3 = this.x); !this.tabs[++e3] && e3 < this._cols; ) ;
        return e3 >= this._cols ? this._cols - 1 : e3 < 0 ? 0 : e3;
      }
      clearMarkers(e3) {
        this._isClearing = true;
        for (let t3 = 0; t3 < this.markers.length; t3++) this.markers[t3].line === e3 && (this.markers[t3].dispose(), this.markers.splice(t3--, 1));
        this._isClearing = false;
      }
      clearAllMarkers() {
        this._isClearing = true;
        for (let e3 = 0; e3 < this.markers.length; e3++) this.markers[e3].dispose();
        this.markers.length = 0, this._isClearing = false;
      }
      addMarker(e3) {
        const t3 = new l.Marker(e3);
        return this.markers.push(t3), t3.register(this.lines.onTrim(((e4) => {
          t3.line -= e4, t3.line < 0 && t3.dispose();
        }))), t3.register(this.lines.onInsert(((e4) => {
          t3.line >= e4.index && (t3.line += e4.amount);
        }))), t3.register(this.lines.onDelete(((e4) => {
          t3.line >= e4.index && t3.line < e4.index + e4.amount && t3.dispose(), t3.line > e4.index && (t3.line -= e4.amount);
        }))), t3.register(t3.onDispose((() => this._removeMarker(t3)))), t3;
      }
      _removeMarker(e3) {
        this._isClearing || this.markers.splice(this.markers.indexOf(e3), 1);
      }
    };
  }, 6107: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.BufferLine = t2.DEFAULT_ATTR_DATA = void 0;
    const i2 = s2(5451), r2 = s2(3055), n2 = s2(8938), o = s2(726);
    t2.DEFAULT_ATTR_DATA = Object.freeze(new i2.AttributeData());
    let a = 0;
    class h {
      constructor(e3, t3, s3 = false) {
        this.isWrapped = s3, this._combined = {}, this._extendedAttrs = {}, this._data = new Uint32Array(3 * e3);
        const i3 = t3 || r2.CellData.fromCharData([0, n2.NULL_CELL_CHAR, n2.NULL_CELL_WIDTH, n2.NULL_CELL_CODE]);
        for (let t4 = 0; t4 < e3; ++t4) this.setCell(t4, i3);
        this.length = e3;
      }
      get(e3) {
        const t3 = this._data[3 * e3 + 0], s3 = 2097151 & t3;
        return [this._data[3 * e3 + 1], 2097152 & t3 ? this._combined[e3] : s3 ? (0, o.stringFromCodePoint)(s3) : "", t3 >> 22, 2097152 & t3 ? this._combined[e3].charCodeAt(this._combined[e3].length - 1) : s3];
      }
      set(e3, t3) {
        this._data[3 * e3 + 1] = t3[n2.CHAR_DATA_ATTR_INDEX], t3[n2.CHAR_DATA_CHAR_INDEX].length > 1 ? (this._combined[e3] = t3[1], this._data[3 * e3 + 0] = 2097152 | e3 | t3[n2.CHAR_DATA_WIDTH_INDEX] << 22) : this._data[3 * e3 + 0] = t3[n2.CHAR_DATA_CHAR_INDEX].charCodeAt(0) | t3[n2.CHAR_DATA_WIDTH_INDEX] << 22;
      }
      getWidth(e3) {
        return this._data[3 * e3 + 0] >> 22;
      }
      hasWidth(e3) {
        return 12582912 & this._data[3 * e3 + 0];
      }
      getFg(e3) {
        return this._data[3 * e3 + 1];
      }
      getBg(e3) {
        return this._data[3 * e3 + 2];
      }
      hasContent(e3) {
        return 4194303 & this._data[3 * e3 + 0];
      }
      getCodePoint(e3) {
        const t3 = this._data[3 * e3 + 0];
        return 2097152 & t3 ? this._combined[e3].charCodeAt(this._combined[e3].length - 1) : 2097151 & t3;
      }
      isCombined(e3) {
        return 2097152 & this._data[3 * e3 + 0];
      }
      getString(e3) {
        const t3 = this._data[3 * e3 + 0];
        return 2097152 & t3 ? this._combined[e3] : 2097151 & t3 ? (0, o.stringFromCodePoint)(2097151 & t3) : "";
      }
      isProtected(e3) {
        return 536870912 & this._data[3 * e3 + 2];
      }
      loadCell(e3, t3) {
        return a = 3 * e3, t3.content = this._data[a + 0], t3.fg = this._data[a + 1], t3.bg = this._data[a + 2], 2097152 & t3.content && (t3.combinedData = this._combined[e3]), 268435456 & t3.bg && (t3.extended = this._extendedAttrs[e3]), t3;
      }
      setCell(e3, t3) {
        2097152 & t3.content && (this._combined[e3] = t3.combinedData), 268435456 & t3.bg && (this._extendedAttrs[e3] = t3.extended), this._data[3 * e3 + 0] = t3.content, this._data[3 * e3 + 1] = t3.fg, this._data[3 * e3 + 2] = t3.bg;
      }
      setCellFromCodepoint(e3, t3, s3, i3) {
        268435456 & i3.bg && (this._extendedAttrs[e3] = i3.extended), this._data[3 * e3 + 0] = t3 | s3 << 22, this._data[3 * e3 + 1] = i3.fg, this._data[3 * e3 + 2] = i3.bg;
      }
      addCodepointToCell(e3, t3, s3) {
        let i3 = this._data[3 * e3 + 0];
        2097152 & i3 ? this._combined[e3] += (0, o.stringFromCodePoint)(t3) : 2097151 & i3 ? (this._combined[e3] = (0, o.stringFromCodePoint)(2097151 & i3) + (0, o.stringFromCodePoint)(t3), i3 &= -2097152, i3 |= 2097152) : i3 = t3 | 1 << 22, s3 && (i3 &= -12582913, i3 |= s3 << 22), this._data[3 * e3 + 0] = i3;
      }
      insertCells(e3, t3, s3) {
        if ((e3 %= this.length) && 2 === this.getWidth(e3 - 1) && this.setCellFromCodepoint(e3 - 1, 0, 1, s3), t3 < this.length - e3) {
          const i3 = new r2.CellData();
          for (let s4 = this.length - e3 - t3 - 1; s4 >= 0; --s4) this.setCell(e3 + t3 + s4, this.loadCell(e3 + s4, i3));
          for (let i4 = 0; i4 < t3; ++i4) this.setCell(e3 + i4, s3);
        } else for (let t4 = e3; t4 < this.length; ++t4) this.setCell(t4, s3);
        2 === this.getWidth(this.length - 1) && this.setCellFromCodepoint(this.length - 1, 0, 1, s3);
      }
      deleteCells(e3, t3, s3) {
        if (e3 %= this.length, t3 < this.length - e3) {
          const i3 = new r2.CellData();
          for (let s4 = 0; s4 < this.length - e3 - t3; ++s4) this.setCell(e3 + s4, this.loadCell(e3 + t3 + s4, i3));
          for (let e4 = this.length - t3; e4 < this.length; ++e4) this.setCell(e4, s3);
        } else for (let t4 = e3; t4 < this.length; ++t4) this.setCell(t4, s3);
        e3 && 2 === this.getWidth(e3 - 1) && this.setCellFromCodepoint(e3 - 1, 0, 1, s3), 0 !== this.getWidth(e3) || this.hasContent(e3) || this.setCellFromCodepoint(e3, 0, 1, s3);
      }
      replaceCells(e3, t3, s3, i3 = false) {
        if (i3) for (e3 && 2 === this.getWidth(e3 - 1) && !this.isProtected(e3 - 1) && this.setCellFromCodepoint(e3 - 1, 0, 1, s3), t3 < this.length && 2 === this.getWidth(t3 - 1) && !this.isProtected(t3) && this.setCellFromCodepoint(t3, 0, 1, s3); e3 < t3 && e3 < this.length; ) this.isProtected(e3) || this.setCell(e3, s3), e3++;
        else for (e3 && 2 === this.getWidth(e3 - 1) && this.setCellFromCodepoint(e3 - 1, 0, 1, s3), t3 < this.length && 2 === this.getWidth(t3 - 1) && this.setCellFromCodepoint(t3, 0, 1, s3); e3 < t3 && e3 < this.length; ) this.setCell(e3++, s3);
      }
      resize(e3, t3) {
        if (e3 === this.length) return 4 * this._data.length * 2 < this._data.buffer.byteLength;
        const s3 = 3 * e3;
        if (e3 > this.length) {
          if (this._data.buffer.byteLength >= 4 * s3) this._data = new Uint32Array(this._data.buffer, 0, s3);
          else {
            const e4 = new Uint32Array(s3);
            e4.set(this._data), this._data = e4;
          }
          for (let s4 = this.length; s4 < e3; ++s4) this.setCell(s4, t3);
        } else {
          this._data = this._data.subarray(0, s3);
          const t4 = Object.keys(this._combined);
          for (let s4 = 0; s4 < t4.length; s4++) {
            const i4 = parseInt(t4[s4], 10);
            i4 >= e3 && delete this._combined[i4];
          }
          const i3 = Object.keys(this._extendedAttrs);
          for (let t5 = 0; t5 < i3.length; t5++) {
            const s4 = parseInt(i3[t5], 10);
            s4 >= e3 && delete this._extendedAttrs[s4];
          }
        }
        return this.length = e3, 4 * s3 * 2 < this._data.buffer.byteLength;
      }
      cleanupMemory() {
        if (4 * this._data.length * 2 < this._data.buffer.byteLength) {
          const e3 = new Uint32Array(this._data.length);
          return e3.set(this._data), this._data = e3, 1;
        }
        return 0;
      }
      fill(e3, t3 = false) {
        if (t3) for (let t4 = 0; t4 < this.length; ++t4) this.isProtected(t4) || this.setCell(t4, e3);
        else {
          this._combined = {}, this._extendedAttrs = {};
          for (let t4 = 0; t4 < this.length; ++t4) this.setCell(t4, e3);
        }
      }
      copyFrom(e3) {
        this.length !== e3.length ? this._data = new Uint32Array(e3._data) : this._data.set(e3._data), this.length = e3.length, this._combined = {};
        for (const t3 in e3._combined) this._combined[t3] = e3._combined[t3];
        this._extendedAttrs = {};
        for (const t3 in e3._extendedAttrs) this._extendedAttrs[t3] = e3._extendedAttrs[t3];
        this.isWrapped = e3.isWrapped;
      }
      clone() {
        const e3 = new h(0);
        e3._data = new Uint32Array(this._data), e3.length = this.length;
        for (const t3 in this._combined) e3._combined[t3] = this._combined[t3];
        for (const t3 in this._extendedAttrs) e3._extendedAttrs[t3] = this._extendedAttrs[t3];
        return e3.isWrapped = this.isWrapped, e3;
      }
      getTrimmedLength() {
        for (let e3 = this.length - 1; e3 >= 0; --e3) if (4194303 & this._data[3 * e3 + 0]) return e3 + (this._data[3 * e3 + 0] >> 22);
        return 0;
      }
      getNoBgTrimmedLength() {
        for (let e3 = this.length - 1; e3 >= 0; --e3) if (4194303 & this._data[3 * e3 + 0] || 50331648 & this._data[3 * e3 + 2]) return e3 + (this._data[3 * e3 + 0] >> 22);
        return 0;
      }
      copyCellsFrom(e3, t3, s3, i3, r3) {
        const n3 = e3._data;
        if (r3) for (let r4 = i3 - 1; r4 >= 0; r4--) {
          for (let e4 = 0; e4 < 3; e4++) this._data[3 * (s3 + r4) + e4] = n3[3 * (t3 + r4) + e4];
          268435456 & n3[3 * (t3 + r4) + 2] && (this._extendedAttrs[s3 + r4] = e3._extendedAttrs[t3 + r4]);
        }
        else for (let r4 = 0; r4 < i3; r4++) {
          for (let e4 = 0; e4 < 3; e4++) this._data[3 * (s3 + r4) + e4] = n3[3 * (t3 + r4) + e4];
          268435456 & n3[3 * (t3 + r4) + 2] && (this._extendedAttrs[s3 + r4] = e3._extendedAttrs[t3 + r4]);
        }
        const o2 = Object.keys(e3._combined);
        for (let i4 = 0; i4 < o2.length; i4++) {
          const r4 = parseInt(o2[i4], 10);
          r4 >= t3 && (this._combined[r4 - t3 + s3] = e3._combined[r4]);
        }
      }
      translateToString(e3, t3, s3, i3) {
        t3 = t3 != null ? t3 : 0, s3 = s3 != null ? s3 : this.length, e3 && (s3 = Math.min(s3, this.getTrimmedLength())), i3 && (i3.length = 0);
        let r3 = "";
        for (; t3 < s3; ) {
          const e4 = this._data[3 * t3 + 0], s4 = 2097151 & e4, a2 = 2097152 & e4 ? this._combined[t3] : s4 ? (0, o.stringFromCodePoint)(s4) : n2.WHITESPACE_CELL_CHAR;
          if (r3 += a2, i3) for (let e5 = 0; e5 < a2.length; ++e5) i3.push(t3);
          t3 += e4 >> 22 || 1;
        }
        return i3 && i3.push(t3), r3;
      }
    }
    t2.BufferLine = h;
  }, 732: (e2, t2) => {
    function s2(e3, t3, s3) {
      if (t3 === e3.length - 1) return e3[t3].getTrimmedLength();
      const i2 = !e3[t3].hasContent(s3 - 1) && 1 === e3[t3].getWidth(s3 - 1), r2 = 2 === e3[t3 + 1].getWidth(0);
      return i2 && r2 ? s3 - 1 : s3;
    }
    Object.defineProperty(t2, "__esModule", { value: true }), t2.reflowLargerGetLinesToRemove = function(e3, t3, i2, r2, n2, o) {
      const a = [];
      for (let h = 0; h < e3.length - 1; h++) {
        let c = h, l = e3.get(++c);
        if (!l.isWrapped) continue;
        const u = [e3.get(h)];
        for (; c < e3.length && l.isWrapped; ) u.push(l), l = e3.get(++c);
        if (!o && r2 >= h && r2 < c) {
          h += u.length - 1;
          continue;
        }
        let d = 0, f = s2(u, d, t3), _ = 1, p = 0;
        for (; _ < u.length; ) {
          const e4 = s2(u, _, t3), r3 = e4 - p, o2 = i2 - f, a2 = Math.min(r3, o2);
          u[d].copyCellsFrom(u[_], p, f, a2, false), f += a2, f === i2 && (d++, f = 0), p += a2, p === e4 && (_++, p = 0), 0 === f && 0 !== d && 2 === u[d - 1].getWidth(i2 - 1) && (u[d].copyCellsFrom(u[d - 1], i2 - 1, f++, 1, false), u[d - 1].setCell(i2 - 1, n2));
        }
        u[d].replaceCells(f, i2, n2);
        let g = 0;
        for (let e4 = u.length - 1; e4 > 0 && (e4 > d || 0 === u[e4].getTrimmedLength()); e4--) g++;
        g > 0 && (a.push(h + u.length - g), a.push(g)), h += u.length - 1;
      }
      return a;
    }, t2.reflowLargerCreateNewLayout = function(e3, t3) {
      const s3 = [];
      let i2 = 0, r2 = t3[i2], n2 = 0;
      for (let o = 0; o < e3.length; o++) if (r2 === o) {
        const s4 = t3[++i2];
        e3.onDeleteEmitter.fire({ index: o - n2, amount: s4 }), o += s4 - 1, n2 += s4, r2 = t3[++i2];
      } else s3.push(o);
      return { layout: s3, countRemoved: n2 };
    }, t2.reflowLargerApplyNewLayout = function(e3, t3) {
      const s3 = [];
      for (let i2 = 0; i2 < t3.length; i2++) s3.push(e3.get(t3[i2]));
      for (let t4 = 0; t4 < s3.length; t4++) e3.set(t4, s3[t4]);
      e3.length = t3.length;
    }, t2.reflowSmallerGetNewLineLengths = function(e3, t3, i2) {
      const r2 = [], n2 = e3.map(((i3, r3) => s2(e3, r3, t3))).reduce(((e4, t4) => e4 + t4));
      let o = 0, a = 0, h = 0;
      for (; h < n2; ) {
        if (n2 - h < i2) {
          r2.push(n2 - h);
          break;
        }
        o += i2;
        const c = s2(e3, a, t3);
        o > c && (o -= c, a++);
        const l = 2 === e3[a].getWidth(o - 1);
        l && o--;
        const u = l ? i2 - 1 : i2;
        r2.push(u), h += u;
      }
      return r2;
    }, t2.getWrappedLineTrimmedLength = s2;
  }, 4097: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.BufferSet = void 0;
    const i2 = s2(7150), r2 = s2(1073), n2 = s2(802);
    class o extends i2.Disposable {
      constructor(e3, t3) {
        super(), this._optionsService = e3, this._bufferService = t3, this._onBufferActivate = this._register(new n2.Emitter()), this.onBufferActivate = this._onBufferActivate.event, this.reset(), this._register(this._optionsService.onSpecificOptionChange("scrollback", (() => this.resize(this._bufferService.cols, this._bufferService.rows)))), this._register(this._optionsService.onSpecificOptionChange("tabStopWidth", (() => this.setupTabStops())));
      }
      reset() {
        this._normal = new r2.Buffer(true, this._optionsService, this._bufferService), this._normal.fillViewportRows(), this._alt = new r2.Buffer(false, this._optionsService, this._bufferService), this._activeBuffer = this._normal, this._onBufferActivate.fire({ activeBuffer: this._normal, inactiveBuffer: this._alt }), this.setupTabStops();
      }
      get alt() {
        return this._alt;
      }
      get active() {
        return this._activeBuffer;
      }
      get normal() {
        return this._normal;
      }
      activateNormalBuffer() {
        this._activeBuffer !== this._normal && (this._normal.x = this._alt.x, this._normal.y = this._alt.y, this._alt.clearAllMarkers(), this._alt.clear(), this._activeBuffer = this._normal, this._onBufferActivate.fire({ activeBuffer: this._normal, inactiveBuffer: this._alt }));
      }
      activateAltBuffer(e3) {
        this._activeBuffer !== this._alt && (this._alt.fillViewportRows(e3), this._alt.x = this._normal.x, this._alt.y = this._normal.y, this._activeBuffer = this._alt, this._onBufferActivate.fire({ activeBuffer: this._alt, inactiveBuffer: this._normal }));
      }
      resize(e3, t3) {
        this._normal.resize(e3, t3), this._alt.resize(e3, t3), this.setupTabStops(e3);
      }
      setupTabStops(e3) {
        this._normal.setupTabStops(e3), this._alt.setupTabStops(e3);
      }
    }
    t2.BufferSet = o;
  }, 3055: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.CellData = void 0;
    const i2 = s2(726), r2 = s2(8938), n2 = s2(5451);
    class o extends n2.AttributeData {
      constructor() {
        super(...arguments), this.content = 0, this.fg = 0, this.bg = 0, this.extended = new n2.ExtendedAttrs(), this.combinedData = "";
      }
      static fromCharData(e3) {
        const t3 = new o();
        return t3.setFromCharData(e3), t3;
      }
      isCombined() {
        return 2097152 & this.content;
      }
      getWidth() {
        return this.content >> 22;
      }
      getChars() {
        return 2097152 & this.content ? this.combinedData : 2097151 & this.content ? (0, i2.stringFromCodePoint)(2097151 & this.content) : "";
      }
      getCode() {
        return this.isCombined() ? this.combinedData.charCodeAt(this.combinedData.length - 1) : 2097151 & this.content;
      }
      setFromCharData(e3) {
        this.fg = e3[r2.CHAR_DATA_ATTR_INDEX], this.bg = 0;
        let t3 = false;
        if (e3[r2.CHAR_DATA_CHAR_INDEX].length > 2) t3 = true;
        else if (2 === e3[r2.CHAR_DATA_CHAR_INDEX].length) {
          const s3 = e3[r2.CHAR_DATA_CHAR_INDEX].charCodeAt(0);
          if (55296 <= s3 && s3 <= 56319) {
            const i3 = e3[r2.CHAR_DATA_CHAR_INDEX].charCodeAt(1);
            56320 <= i3 && i3 <= 57343 ? this.content = 1024 * (s3 - 55296) + i3 - 56320 + 65536 | e3[r2.CHAR_DATA_WIDTH_INDEX] << 22 : t3 = true;
          } else t3 = true;
        } else this.content = e3[r2.CHAR_DATA_CHAR_INDEX].charCodeAt(0) | e3[r2.CHAR_DATA_WIDTH_INDEX] << 22;
        t3 && (this.combinedData = e3[r2.CHAR_DATA_CHAR_INDEX], this.content = 2097152 | e3[r2.CHAR_DATA_WIDTH_INDEX] << 22);
      }
      getAsCharData() {
        return [this.fg, this.getChars(), this.getWidth(), this.getCode()];
      }
    }
    t2.CellData = o;
  }, 8938: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.WHITESPACE_CELL_CODE = t2.WHITESPACE_CELL_WIDTH = t2.WHITESPACE_CELL_CHAR = t2.NULL_CELL_CODE = t2.NULL_CELL_WIDTH = t2.NULL_CELL_CHAR = t2.CHAR_DATA_CODE_INDEX = t2.CHAR_DATA_WIDTH_INDEX = t2.CHAR_DATA_CHAR_INDEX = t2.CHAR_DATA_ATTR_INDEX = t2.DEFAULT_EXT = t2.DEFAULT_ATTR = t2.DEFAULT_COLOR = void 0, t2.DEFAULT_COLOR = 0, t2.DEFAULT_ATTR = t2.DEFAULT_COLOR << 9 | 256, t2.DEFAULT_EXT = 0, t2.CHAR_DATA_ATTR_INDEX = 0, t2.CHAR_DATA_CHAR_INDEX = 1, t2.CHAR_DATA_WIDTH_INDEX = 2, t2.CHAR_DATA_CODE_INDEX = 3, t2.NULL_CELL_CHAR = "", t2.NULL_CELL_WIDTH = 1, t2.NULL_CELL_CODE = 0, t2.WHITESPACE_CELL_CHAR = " ", t2.WHITESPACE_CELL_WIDTH = 1, t2.WHITESPACE_CELL_CODE = 32;
  }, 8158: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.Marker = void 0;
    const i2 = s2(802), r2 = s2(7150);
    class n2 {
      get id() {
        return this._id;
      }
      constructor(e3) {
        this.line = e3, this.isDisposed = false, this._disposables = [], this._id = n2._nextId++, this._onDispose = this.register(new i2.Emitter()), this.onDispose = this._onDispose.event;
      }
      dispose() {
        this.isDisposed || (this.isDisposed = true, this.line = -1, this._onDispose.fire(), (0, r2.dispose)(this._disposables), this._disposables.length = 0);
      }
      register(e3) {
        return this._disposables.push(e3), e3;
      }
    }
    t2.Marker = n2, n2._nextId = 1;
  }, 6760: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.DEFAULT_CHARSET = t2.CHARSETS = void 0, t2.CHARSETS = {}, t2.DEFAULT_CHARSET = t2.CHARSETS.B, t2.CHARSETS[0] = { "`": "\u25C6", a: "\u2592", b: "\u2409", c: "\u240C", d: "\u240D", e: "\u240A", f: "\xB0", g: "\xB1", h: "\u2424", i: "\u240B", j: "\u2518", k: "\u2510", l: "\u250C", m: "\u2514", n: "\u253C", o: "\u23BA", p: "\u23BB", q: "\u2500", r: "\u23BC", s: "\u23BD", t: "\u251C", u: "\u2524", v: "\u2534", w: "\u252C", x: "\u2502", y: "\u2264", z: "\u2265", "{": "\u03C0", "|": "\u2260", "}": "\xA3", "~": "\xB7" }, t2.CHARSETS.A = { "#": "\xA3" }, t2.CHARSETS.B = void 0, t2.CHARSETS[4] = { "#": "\xA3", "@": "\xBE", "[": "ij", "\\": "\xBD", "]": "|", "{": "\xA8", "|": "f", "}": "\xBC", "~": "\xB4" }, t2.CHARSETS.C = t2.CHARSETS[5] = { "[": "\xC4", "\\": "\xD6", "]": "\xC5", "^": "\xDC", "`": "\xE9", "{": "\xE4", "|": "\xF6", "}": "\xE5", "~": "\xFC" }, t2.CHARSETS.R = { "#": "\xA3", "@": "\xE0", "[": "\xB0", "\\": "\xE7", "]": "\xA7", "{": "\xE9", "|": "\xF9", "}": "\xE8", "~": "\xA8" }, t2.CHARSETS.Q = { "@": "\xE0", "[": "\xE2", "\\": "\xE7", "]": "\xEA", "^": "\xEE", "`": "\xF4", "{": "\xE9", "|": "\xF9", "}": "\xE8", "~": "\xFB" }, t2.CHARSETS.K = { "@": "\xA7", "[": "\xC4", "\\": "\xD6", "]": "\xDC", "{": "\xE4", "|": "\xF6", "}": "\xFC", "~": "\xDF" }, t2.CHARSETS.Y = { "#": "\xA3", "@": "\xA7", "[": "\xB0", "\\": "\xE7", "]": "\xE9", "`": "\xF9", "{": "\xE0", "|": "\xF2", "}": "\xE8", "~": "\xEC" }, t2.CHARSETS.E = t2.CHARSETS[6] = { "@": "\xC4", "[": "\xC6", "\\": "\xD8", "]": "\xC5", "^": "\xDC", "`": "\xE4", "{": "\xE6", "|": "\xF8", "}": "\xE5", "~": "\xFC" }, t2.CHARSETS.Z = { "#": "\xA3", "@": "\xA7", "[": "\xA1", "\\": "\xD1", "]": "\xBF", "{": "\xB0", "|": "\xF1", "}": "\xE7" }, t2.CHARSETS.H = t2.CHARSETS[7] = { "@": "\xC9", "[": "\xC4", "\\": "\xD6", "]": "\xC5", "^": "\xDC", "`": "\xE9", "{": "\xE4", "|": "\xF6", "}": "\xE5", "~": "\xFC" }, t2.CHARSETS["="] = { "#": "\xF9", "@": "\xE0", "[": "\xE9", "\\": "\xE7", "]": "\xEA", "^": "\xEE", _: "\xE8", "`": "\xF4", "{": "\xE4", "|": "\xF6", "}": "\xFC", "~": "\xFB" };
  }, 3534: (e2, t2) => {
    var s2, i2, r2;
    Object.defineProperty(t2, "__esModule", { value: true }), t2.C1_ESCAPED = t2.C1 = t2.C0 = void 0, (function(e3) {
      e3.NUL = "\0", e3.SOH = "", e3.STX = "", e3.ETX = "", e3.EOT = "", e3.ENQ = "", e3.ACK = "", e3.BEL = "\x07", e3.BS = "\b", e3.HT = "	", e3.LF = "\n", e3.VT = "\v", e3.FF = "\f", e3.CR = "\r", e3.SO = "", e3.SI = "", e3.DLE = "", e3.DC1 = "", e3.DC2 = "", e3.DC3 = "", e3.DC4 = "", e3.NAK = "", e3.SYN = "", e3.ETB = "", e3.CAN = "", e3.EM = "", e3.SUB = "", e3.ESC = "\x1B", e3.FS = "", e3.GS = "", e3.RS = "", e3.US = "", e3.SP = " ", e3.DEL = "\x7F";
    })(s2 || (t2.C0 = s2 = {})), (function(e3) {
      e3.PAD = "\x80", e3.HOP = "\x81", e3.BPH = "\x82", e3.NBH = "\x83", e3.IND = "\x84", e3.NEL = "\x85", e3.SSA = "\x86", e3.ESA = "\x87", e3.HTS = "\x88", e3.HTJ = "\x89", e3.VTS = "\x8A", e3.PLD = "\x8B", e3.PLU = "\x8C", e3.RI = "\x8D", e3.SS2 = "\x8E", e3.SS3 = "\x8F", e3.DCS = "\x90", e3.PU1 = "\x91", e3.PU2 = "\x92", e3.STS = "\x93", e3.CCH = "\x94", e3.MW = "\x95", e3.SPA = "\x96", e3.EPA = "\x97", e3.SOS = "\x98", e3.SGCI = "\x99", e3.SCI = "\x9A", e3.CSI = "\x9B", e3.ST = "\x9C", e3.OSC = "\x9D", e3.PM = "\x9E", e3.APC = "\x9F";
    })(i2 || (t2.C1 = i2 = {})), (function(e3) {
      e3.ST = `${s2.ESC}\\`;
    })(r2 || (t2.C1_ESCAPED = r2 = {}));
  }, 726: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.Utf8ToUtf32 = t2.StringToUtf32 = void 0, t2.stringFromCodePoint = function(e3) {
      return e3 > 65535 ? (e3 -= 65536, String.fromCharCode(55296 + (e3 >> 10)) + String.fromCharCode(e3 % 1024 + 56320)) : String.fromCharCode(e3);
    }, t2.utf32ToString = function(e3, t3 = 0, s2 = e3.length) {
      let i2 = "";
      for (let r2 = t3; r2 < s2; ++r2) {
        let t4 = e3[r2];
        t4 > 65535 ? (t4 -= 65536, i2 += String.fromCharCode(55296 + (t4 >> 10)) + String.fromCharCode(t4 % 1024 + 56320)) : i2 += String.fromCharCode(t4);
      }
      return i2;
    }, t2.StringToUtf32 = class {
      constructor() {
        this._interim = 0;
      }
      clear() {
        this._interim = 0;
      }
      decode(e3, t3) {
        const s2 = e3.length;
        if (!s2) return 0;
        let i2 = 0, r2 = 0;
        if (this._interim) {
          const s3 = e3.charCodeAt(r2++);
          56320 <= s3 && s3 <= 57343 ? t3[i2++] = 1024 * (this._interim - 55296) + s3 - 56320 + 65536 : (t3[i2++] = this._interim, t3[i2++] = s3), this._interim = 0;
        }
        for (let n2 = r2; n2 < s2; ++n2) {
          const r3 = e3.charCodeAt(n2);
          if (55296 <= r3 && r3 <= 56319) {
            if (++n2 >= s2) return this._interim = r3, i2;
            const o = e3.charCodeAt(n2);
            56320 <= o && o <= 57343 ? t3[i2++] = 1024 * (r3 - 55296) + o - 56320 + 65536 : (t3[i2++] = r3, t3[i2++] = o);
          } else 65279 !== r3 && (t3[i2++] = r3);
        }
        return i2;
      }
    }, t2.Utf8ToUtf32 = class {
      constructor() {
        this.interim = new Uint8Array(3);
      }
      clear() {
        this.interim.fill(0);
      }
      decode(e3, t3) {
        const s2 = e3.length;
        if (!s2) return 0;
        let i2, r2, n2, o, a = 0, h = 0, c = 0;
        if (this.interim[0]) {
          let i3 = false, r3 = this.interim[0];
          r3 &= 192 == (224 & r3) ? 31 : 224 == (240 & r3) ? 15 : 7;
          let n3, o2 = 0;
          for (; (n3 = 63 & this.interim[++o2]) && o2 < 4; ) r3 <<= 6, r3 |= n3;
          const h2 = 192 == (224 & this.interim[0]) ? 2 : 224 == (240 & this.interim[0]) ? 3 : 4, l2 = h2 - o2;
          for (; c < l2; ) {
            if (c >= s2) return 0;
            if (n3 = e3[c++], 128 != (192 & n3)) {
              c--, i3 = true;
              break;
            }
            this.interim[o2++] = n3, r3 <<= 6, r3 |= 63 & n3;
          }
          i3 || (2 === h2 ? r3 < 128 ? c-- : t3[a++] = r3 : 3 === h2 ? r3 < 2048 || r3 >= 55296 && r3 <= 57343 || 65279 === r3 || (t3[a++] = r3) : r3 < 65536 || r3 > 1114111 || (t3[a++] = r3)), this.interim.fill(0);
        }
        const l = s2 - 4;
        let u = c;
        for (; u < s2; ) {
          for (; !(!(u < l) || 128 & (i2 = e3[u]) || 128 & (r2 = e3[u + 1]) || 128 & (n2 = e3[u + 2]) || 128 & (o = e3[u + 3])); ) t3[a++] = i2, t3[a++] = r2, t3[a++] = n2, t3[a++] = o, u += 4;
          if (i2 = e3[u++], i2 < 128) t3[a++] = i2;
          else if (192 == (224 & i2)) {
            if (u >= s2) return this.interim[0] = i2, a;
            if (r2 = e3[u++], 128 != (192 & r2)) {
              u--;
              continue;
            }
            if (h = (31 & i2) << 6 | 63 & r2, h < 128) {
              u--;
              continue;
            }
            t3[a++] = h;
          } else if (224 == (240 & i2)) {
            if (u >= s2) return this.interim[0] = i2, a;
            if (r2 = e3[u++], 128 != (192 & r2)) {
              u--;
              continue;
            }
            if (u >= s2) return this.interim[0] = i2, this.interim[1] = r2, a;
            if (n2 = e3[u++], 128 != (192 & n2)) {
              u--;
              continue;
            }
            if (h = (15 & i2) << 12 | (63 & r2) << 6 | 63 & n2, h < 2048 || h >= 55296 && h <= 57343 || 65279 === h) continue;
            t3[a++] = h;
          } else if (240 == (248 & i2)) {
            if (u >= s2) return this.interim[0] = i2, a;
            if (r2 = e3[u++], 128 != (192 & r2)) {
              u--;
              continue;
            }
            if (u >= s2) return this.interim[0] = i2, this.interim[1] = r2, a;
            if (n2 = e3[u++], 128 != (192 & n2)) {
              u--;
              continue;
            }
            if (u >= s2) return this.interim[0] = i2, this.interim[1] = r2, this.interim[2] = n2, a;
            if (o = e3[u++], 128 != (192 & o)) {
              u--;
              continue;
            }
            if (h = (7 & i2) << 18 | (63 & r2) << 12 | (63 & n2) << 6 | 63 & o, h < 65536 || h > 1114111) continue;
            t3[a++] = h;
          }
        }
        return a;
      }
    };
  }, 7428: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.UnicodeV6 = void 0;
    const i2 = s2(6415), r2 = [[768, 879], [1155, 1158], [1160, 1161], [1425, 1469], [1471, 1471], [1473, 1474], [1476, 1477], [1479, 1479], [1536, 1539], [1552, 1557], [1611, 1630], [1648, 1648], [1750, 1764], [1767, 1768], [1770, 1773], [1807, 1807], [1809, 1809], [1840, 1866], [1958, 1968], [2027, 2035], [2305, 2306], [2364, 2364], [2369, 2376], [2381, 2381], [2385, 2388], [2402, 2403], [2433, 2433], [2492, 2492], [2497, 2500], [2509, 2509], [2530, 2531], [2561, 2562], [2620, 2620], [2625, 2626], [2631, 2632], [2635, 2637], [2672, 2673], [2689, 2690], [2748, 2748], [2753, 2757], [2759, 2760], [2765, 2765], [2786, 2787], [2817, 2817], [2876, 2876], [2879, 2879], [2881, 2883], [2893, 2893], [2902, 2902], [2946, 2946], [3008, 3008], [3021, 3021], [3134, 3136], [3142, 3144], [3146, 3149], [3157, 3158], [3260, 3260], [3263, 3263], [3270, 3270], [3276, 3277], [3298, 3299], [3393, 3395], [3405, 3405], [3530, 3530], [3538, 3540], [3542, 3542], [3633, 3633], [3636, 3642], [3655, 3662], [3761, 3761], [3764, 3769], [3771, 3772], [3784, 3789], [3864, 3865], [3893, 3893], [3895, 3895], [3897, 3897], [3953, 3966], [3968, 3972], [3974, 3975], [3984, 3991], [3993, 4028], [4038, 4038], [4141, 4144], [4146, 4146], [4150, 4151], [4153, 4153], [4184, 4185], [4448, 4607], [4959, 4959], [5906, 5908], [5938, 5940], [5970, 5971], [6002, 6003], [6068, 6069], [6071, 6077], [6086, 6086], [6089, 6099], [6109, 6109], [6155, 6157], [6313, 6313], [6432, 6434], [6439, 6440], [6450, 6450], [6457, 6459], [6679, 6680], [6912, 6915], [6964, 6964], [6966, 6970], [6972, 6972], [6978, 6978], [7019, 7027], [7616, 7626], [7678, 7679], [8203, 8207], [8234, 8238], [8288, 8291], [8298, 8303], [8400, 8431], [12330, 12335], [12441, 12442], [43014, 43014], [43019, 43019], [43045, 43046], [64286, 64286], [65024, 65039], [65056, 65059], [65279, 65279], [65529, 65531]], n2 = [[68097, 68099], [68101, 68102], [68108, 68111], [68152, 68154], [68159, 68159], [119143, 119145], [119155, 119170], [119173, 119179], [119210, 119213], [119362, 119364], [917505, 917505], [917536, 917631], [917760, 917999]];
    let o;
    t2.UnicodeV6 = class {
      constructor() {
        if (this.version = "6", !o) {
          o = new Uint8Array(65536), o.fill(1), o[0] = 0, o.fill(0, 1, 32), o.fill(0, 127, 160), o.fill(2, 4352, 4448), o[9001] = 2, o[9002] = 2, o.fill(2, 11904, 42192), o[12351] = 1, o.fill(2, 44032, 55204), o.fill(2, 63744, 64256), o.fill(2, 65040, 65050), o.fill(2, 65072, 65136), o.fill(2, 65280, 65377), o.fill(2, 65504, 65511);
          for (let e3 = 0; e3 < r2.length; ++e3) o.fill(0, r2[e3][0], r2[e3][1] + 1);
        }
      }
      wcwidth(e3) {
        return e3 < 32 ? 0 : e3 < 127 ? 1 : e3 < 65536 ? o[e3] : (function(e4, t3) {
          let s3, i3 = 0, r3 = t3.length - 1;
          if (e4 < t3[0][0] || e4 > t3[r3][1]) return false;
          for (; r3 >= i3; ) if (s3 = i3 + r3 >> 1, e4 > t3[s3][1]) i3 = s3 + 1;
          else {
            if (!(e4 < t3[s3][0])) return true;
            r3 = s3 - 1;
          }
          return false;
        })(e3, n2) ? 0 : e3 >= 131072 && e3 <= 196605 || e3 >= 196608 && e3 <= 262141 ? 2 : 1;
      }
      charProperties(e3, t3) {
        let s3 = this.wcwidth(e3), r3 = 0 === s3 && 0 !== t3;
        if (r3) {
          const e4 = i2.UnicodeService.extractWidth(t3);
          0 === e4 ? r3 = false : e4 > s3 && (s3 = e4);
        }
        return i2.UnicodeService.createPropertyValue(0, s3, r3);
      }
    };
  }, 3562: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.WriteBuffer = void 0;
    const i2 = s2(7150), r2 = s2(802);
    class n2 extends i2.Disposable {
      constructor(e3) {
        super(), this._action = e3, this._writeBuffer = [], this._callbacks = [], this._pendingData = 0, this._bufferOffset = 0, this._isSyncWriting = false, this._syncCalls = 0, this._didUserInput = false, this._onWriteParsed = this._register(new r2.Emitter()), this.onWriteParsed = this._onWriteParsed.event;
      }
      handleUserInput() {
        this._didUserInput = true;
      }
      writeSync(e3, t3) {
        if (void 0 !== t3 && this._syncCalls > t3) return void (this._syncCalls = 0);
        if (this._pendingData += e3.length, this._writeBuffer.push(e3), this._callbacks.push(void 0), this._syncCalls++, this._isSyncWriting) return;
        let s3;
        for (this._isSyncWriting = true; s3 = this._writeBuffer.shift(); ) {
          this._action(s3);
          const e4 = this._callbacks.shift();
          e4 && e4();
        }
        this._pendingData = 0, this._bufferOffset = 2147483647, this._isSyncWriting = false, this._syncCalls = 0;
      }
      write(e3, t3) {
        if (this._pendingData > 5e7) throw new Error("write data discarded, use flow control to avoid losing data");
        if (!this._writeBuffer.length) {
          if (this._bufferOffset = 0, this._didUserInput) return this._didUserInput = false, this._pendingData += e3.length, this._writeBuffer.push(e3), this._callbacks.push(t3), void this._innerWrite();
          setTimeout((() => this._innerWrite()));
        }
        this._pendingData += e3.length, this._writeBuffer.push(e3), this._callbacks.push(t3);
      }
      _innerWrite(e3 = 0, t3 = true) {
        const s3 = e3 || performance.now();
        for (; this._writeBuffer.length > this._bufferOffset; ) {
          const e4 = this._writeBuffer[this._bufferOffset], i3 = this._action(e4, t3);
          if (i3) {
            const e5 = (e6) => performance.now() - s3 >= 12 ? setTimeout((() => this._innerWrite(0, e6))) : this._innerWrite(s3, e6);
            return void i3.catch(((e6) => (queueMicrotask((() => {
              throw e6;
            })), Promise.resolve(false)))).then(e5);
          }
          const r3 = this._callbacks[this._bufferOffset];
          if (r3 && r3(), this._bufferOffset++, this._pendingData -= e4.length, performance.now() - s3 >= 12) break;
        }
        this._writeBuffer.length > this._bufferOffset ? (this._bufferOffset > 50 && (this._writeBuffer = this._writeBuffer.slice(this._bufferOffset), this._callbacks = this._callbacks.slice(this._bufferOffset), this._bufferOffset = 0), setTimeout((() => this._innerWrite()))) : (this._writeBuffer.length = 0, this._callbacks.length = 0, this._pendingData = 0, this._bufferOffset = 0), this._onWriteParsed.fire();
      }
    }
    t2.WriteBuffer = n2;
  }, 8693: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.parseColor = function(e3) {
      if (!e3) return;
      let t3 = e3.toLowerCase();
      if (0 === t3.indexOf("rgb:")) {
        t3 = t3.slice(4);
        const e4 = s2.exec(t3);
        if (e4) {
          const t4 = e4[1] ? 15 : e4[4] ? 255 : e4[7] ? 4095 : 65535;
          return [Math.round(parseInt(e4[1] || e4[4] || e4[7] || e4[10], 16) / t4 * 255), Math.round(parseInt(e4[2] || e4[5] || e4[8] || e4[11], 16) / t4 * 255), Math.round(parseInt(e4[3] || e4[6] || e4[9] || e4[12], 16) / t4 * 255)];
        }
      } else if (0 === t3.indexOf("#") && (t3 = t3.slice(1), i2.exec(t3) && [3, 6, 9, 12].includes(t3.length))) {
        const e4 = t3.length / 3, s3 = [0, 0, 0];
        for (let i3 = 0; i3 < 3; ++i3) {
          const r3 = parseInt(t3.slice(e4 * i3, e4 * i3 + e4), 16);
          s3[i3] = 1 === e4 ? r3 << 4 : 2 === e4 ? r3 : 3 === e4 ? r3 >> 4 : r3 >> 8;
        }
        return s3;
      }
    }, t2.toRgbString = function(e3, t3 = 16) {
      const [s3, i3, n2] = e3;
      return `rgb:${r2(s3, t3)}/${r2(i3, t3)}/${r2(n2, t3)}`;
    };
    const s2 = /^([\da-f])\/([\da-f])\/([\da-f])$|^([\da-f]{2})\/([\da-f]{2})\/([\da-f]{2})$|^([\da-f]{3})\/([\da-f]{3})\/([\da-f]{3})$|^([\da-f]{4})\/([\da-f]{4})\/([\da-f]{4})$/, i2 = /^[\da-f]+$/;
    function r2(e3, t3) {
      const s3 = e3.toString(16), i3 = s3.length < 2 ? "0" + s3 : s3;
      switch (t3) {
        case 4:
          return s3[0];
        case 8:
          return i3;
        case 12:
          return (i3 + i3).slice(0, 3);
        default:
          return i3 + i3;
      }
    }
  }, 1263: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.PAYLOAD_LIMIT = void 0, t2.PAYLOAD_LIMIT = 1e7;
  }, 9823: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.DcsHandler = t2.DcsParser = void 0;
    const i2 = s2(726), r2 = s2(7262), n2 = s2(1263), o = [];
    t2.DcsParser = class {
      constructor() {
        this._handlers = /* @__PURE__ */ Object.create(null), this._active = o, this._ident = 0, this._handlerFb = () => {
        }, this._stack = { paused: false, loopPosition: 0, fallThrough: false };
      }
      dispose() {
        this._handlers = /* @__PURE__ */ Object.create(null), this._handlerFb = () => {
        }, this._active = o;
      }
      registerHandler(e3, t3) {
        void 0 === this._handlers[e3] && (this._handlers[e3] = []);
        const s3 = this._handlers[e3];
        return s3.push(t3), { dispose: () => {
          const e4 = s3.indexOf(t3);
          -1 !== e4 && s3.splice(e4, 1);
        } };
      }
      clearHandler(e3) {
        this._handlers[e3] && delete this._handlers[e3];
      }
      setHandlerFallback(e3) {
        this._handlerFb = e3;
      }
      reset() {
        if (this._active.length) for (let e3 = this._stack.paused ? this._stack.loopPosition - 1 : this._active.length - 1; e3 >= 0; --e3) this._active[e3].unhook(false);
        this._stack.paused = false, this._active = o, this._ident = 0;
      }
      hook(e3, t3) {
        if (this.reset(), this._ident = e3, this._active = this._handlers[e3] || o, this._active.length) for (let e4 = this._active.length - 1; e4 >= 0; e4--) this._active[e4].hook(t3);
        else this._handlerFb(this._ident, "HOOK", t3);
      }
      put(e3, t3, s3) {
        if (this._active.length) for (let i3 = this._active.length - 1; i3 >= 0; i3--) this._active[i3].put(e3, t3, s3);
        else this._handlerFb(this._ident, "PUT", (0, i2.utf32ToString)(e3, t3, s3));
      }
      unhook(e3, t3 = true) {
        if (this._active.length) {
          let s3 = false, i3 = this._active.length - 1, r3 = false;
          if (this._stack.paused && (i3 = this._stack.loopPosition - 1, s3 = t3, r3 = this._stack.fallThrough, this._stack.paused = false), !r3 && false === s3) {
            for (; i3 >= 0 && (s3 = this._active[i3].unhook(e3), true !== s3); i3--) if (s3 instanceof Promise) return this._stack.paused = true, this._stack.loopPosition = i3, this._stack.fallThrough = false, s3;
            i3--;
          }
          for (; i3 >= 0; i3--) if (s3 = this._active[i3].unhook(false), s3 instanceof Promise) return this._stack.paused = true, this._stack.loopPosition = i3, this._stack.fallThrough = true, s3;
        } else this._handlerFb(this._ident, "UNHOOK", e3);
        this._active = o, this._ident = 0;
      }
    };
    const a = new r2.Params();
    a.addParam(0), t2.DcsHandler = class {
      constructor(e3) {
        this._handler = e3, this._data = "", this._params = a, this._hitLimit = false;
      }
      hook(e3) {
        this._params = e3.length > 1 || e3.params[0] ? e3.clone() : a, this._data = "", this._hitLimit = false;
      }
      put(e3, t3, s3) {
        this._hitLimit || (this._data += (0, i2.utf32ToString)(e3, t3, s3), this._data.length > n2.PAYLOAD_LIMIT && (this._data = "", this._hitLimit = true));
      }
      unhook(e3) {
        let t3 = false;
        if (this._hitLimit) t3 = false;
        else if (e3 && (t3 = this._handler(this._data, this._params), t3 instanceof Promise)) return t3.then(((e4) => (this._params = a, this._data = "", this._hitLimit = false, e4)));
        return this._params = a, this._data = "", this._hitLimit = false, t3;
      }
    };
  }, 6717: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.EscapeSequenceParser = t2.VT500_TRANSITION_TABLE = t2.TransitionTable = void 0;
    const i2 = s2(7150), r2 = s2(7262), n2 = s2(1346), o = s2(9823);
    class a {
      constructor(e3) {
        this.table = new Uint8Array(e3);
      }
      setDefault(e3, t3) {
        this.table.fill(e3 << 4 | t3);
      }
      add(e3, t3, s3, i3) {
        this.table[t3 << 8 | e3] = s3 << 4 | i3;
      }
      addMany(e3, t3, s3, i3) {
        for (let r3 = 0; r3 < e3.length; r3++) this.table[t3 << 8 | e3[r3]] = s3 << 4 | i3;
      }
    }
    t2.TransitionTable = a;
    const h = 160;
    t2.VT500_TRANSITION_TABLE = (function() {
      const e3 = new a(4095), t3 = Array.apply(null, Array(256)).map(((e4, t4) => t4)), s3 = (e4, s4) => t3.slice(e4, s4), i3 = s3(32, 127), r3 = s3(0, 24);
      r3.push(25), r3.push.apply(r3, s3(28, 32));
      const n3 = s3(0, 14);
      let o2;
      for (o2 in e3.setDefault(1, 0), e3.addMany(i3, 0, 2, 0), n3) e3.addMany([24, 26, 153, 154], o2, 3, 0), e3.addMany(s3(128, 144), o2, 3, 0), e3.addMany(s3(144, 152), o2, 3, 0), e3.add(156, o2, 0, 0), e3.add(27, o2, 11, 1), e3.add(157, o2, 4, 8), e3.addMany([152, 158, 159], o2, 0, 7), e3.add(155, o2, 11, 3), e3.add(144, o2, 11, 9);
      return e3.addMany(r3, 0, 3, 0), e3.addMany(r3, 1, 3, 1), e3.add(127, 1, 0, 1), e3.addMany(r3, 8, 0, 8), e3.addMany(r3, 3, 3, 3), e3.add(127, 3, 0, 3), e3.addMany(r3, 4, 3, 4), e3.add(127, 4, 0, 4), e3.addMany(r3, 6, 3, 6), e3.addMany(r3, 5, 3, 5), e3.add(127, 5, 0, 5), e3.addMany(r3, 2, 3, 2), e3.add(127, 2, 0, 2), e3.add(93, 1, 4, 8), e3.addMany(i3, 8, 5, 8), e3.add(127, 8, 5, 8), e3.addMany([156, 27, 24, 26, 7], 8, 6, 0), e3.addMany(s3(28, 32), 8, 0, 8), e3.addMany([88, 94, 95], 1, 0, 7), e3.addMany(i3, 7, 0, 7), e3.addMany(r3, 7, 0, 7), e3.add(156, 7, 0, 0), e3.add(127, 7, 0, 7), e3.add(91, 1, 11, 3), e3.addMany(s3(64, 127), 3, 7, 0), e3.addMany(s3(48, 60), 3, 8, 4), e3.addMany([60, 61, 62, 63], 3, 9, 4), e3.addMany(s3(48, 60), 4, 8, 4), e3.addMany(s3(64, 127), 4, 7, 0), e3.addMany([60, 61, 62, 63], 4, 0, 6), e3.addMany(s3(32, 64), 6, 0, 6), e3.add(127, 6, 0, 6), e3.addMany(s3(64, 127), 6, 0, 0), e3.addMany(s3(32, 48), 3, 9, 5), e3.addMany(s3(32, 48), 5, 9, 5), e3.addMany(s3(48, 64), 5, 0, 6), e3.addMany(s3(64, 127), 5, 7, 0), e3.addMany(s3(32, 48), 4, 9, 5), e3.addMany(s3(32, 48), 1, 9, 2), e3.addMany(s3(32, 48), 2, 9, 2), e3.addMany(s3(48, 127), 2, 10, 0), e3.addMany(s3(48, 80), 1, 10, 0), e3.addMany(s3(81, 88), 1, 10, 0), e3.addMany([89, 90, 92], 1, 10, 0), e3.addMany(s3(96, 127), 1, 10, 0), e3.add(80, 1, 11, 9), e3.addMany(r3, 9, 0, 9), e3.add(127, 9, 0, 9), e3.addMany(s3(28, 32), 9, 0, 9), e3.addMany(s3(32, 48), 9, 9, 12), e3.addMany(s3(48, 60), 9, 8, 10), e3.addMany([60, 61, 62, 63], 9, 9, 10), e3.addMany(r3, 11, 0, 11), e3.addMany(s3(32, 128), 11, 0, 11), e3.addMany(s3(28, 32), 11, 0, 11), e3.addMany(r3, 10, 0, 10), e3.add(127, 10, 0, 10), e3.addMany(s3(28, 32), 10, 0, 10), e3.addMany(s3(48, 60), 10, 8, 10), e3.addMany([60, 61, 62, 63], 10, 0, 11), e3.addMany(s3(32, 48), 10, 9, 12), e3.addMany(r3, 12, 0, 12), e3.add(127, 12, 0, 12), e3.addMany(s3(28, 32), 12, 0, 12), e3.addMany(s3(32, 48), 12, 9, 12), e3.addMany(s3(48, 64), 12, 0, 11), e3.addMany(s3(64, 127), 12, 12, 13), e3.addMany(s3(64, 127), 10, 12, 13), e3.addMany(s3(64, 127), 9, 12, 13), e3.addMany(r3, 13, 13, 13), e3.addMany(i3, 13, 13, 13), e3.add(127, 13, 0, 13), e3.addMany([27, 156, 24, 26], 13, 14, 0), e3.add(h, 0, 2, 0), e3.add(h, 8, 5, 8), e3.add(h, 6, 0, 6), e3.add(h, 11, 0, 11), e3.add(h, 13, 13, 13), e3;
    })();
    class c extends i2.Disposable {
      constructor(e3 = t2.VT500_TRANSITION_TABLE) {
        super(), this._transitions = e3, this._parseStack = { state: 0, handlers: [], handlerPos: 0, transition: 0, chunkPos: 0 }, this.initialState = 0, this.currentState = this.initialState, this._params = new r2.Params(), this._params.addParam(0), this._collect = 0, this.precedingJoinState = 0, this._printHandlerFb = (e4, t3, s3) => {
        }, this._executeHandlerFb = (e4) => {
        }, this._csiHandlerFb = (e4, t3) => {
        }, this._escHandlerFb = (e4) => {
        }, this._errorHandlerFb = (e4) => e4, this._printHandler = this._printHandlerFb, this._executeHandlers = /* @__PURE__ */ Object.create(null), this._csiHandlers = /* @__PURE__ */ Object.create(null), this._escHandlers = /* @__PURE__ */ Object.create(null), this._register((0, i2.toDisposable)((() => {
          this._csiHandlers = /* @__PURE__ */ Object.create(null), this._executeHandlers = /* @__PURE__ */ Object.create(null), this._escHandlers = /* @__PURE__ */ Object.create(null);
        }))), this._oscParser = this._register(new n2.OscParser()), this._dcsParser = this._register(new o.DcsParser()), this._errorHandler = this._errorHandlerFb, this.registerEscHandler({ final: "\\" }, (() => true));
      }
      _identifier(e3, t3 = [64, 126]) {
        let s3 = 0;
        if (e3.prefix) {
          if (e3.prefix.length > 1) throw new Error("only one byte as prefix supported");
          if (s3 = e3.prefix.charCodeAt(0), s3 && 60 > s3 || s3 > 63) throw new Error("prefix must be in range 0x3c .. 0x3f");
        }
        if (e3.intermediates) {
          if (e3.intermediates.length > 2) throw new Error("only two bytes as intermediates are supported");
          for (let t4 = 0; t4 < e3.intermediates.length; ++t4) {
            const i4 = e3.intermediates.charCodeAt(t4);
            if (32 > i4 || i4 > 47) throw new Error("intermediate must be in range 0x20 .. 0x2f");
            s3 <<= 8, s3 |= i4;
          }
        }
        if (1 !== e3.final.length) throw new Error("final must be a single byte");
        const i3 = e3.final.charCodeAt(0);
        if (t3[0] > i3 || i3 > t3[1]) throw new Error(`final must be in range ${t3[0]} .. ${t3[1]}`);
        return s3 <<= 8, s3 |= i3, s3;
      }
      identToString(e3) {
        const t3 = [];
        for (; e3; ) t3.push(String.fromCharCode(255 & e3)), e3 >>= 8;
        return t3.reverse().join("");
      }
      setPrintHandler(e3) {
        this._printHandler = e3;
      }
      clearPrintHandler() {
        this._printHandler = this._printHandlerFb;
      }
      registerEscHandler(e3, t3) {
        const s3 = this._identifier(e3, [48, 126]);
        void 0 === this._escHandlers[s3] && (this._escHandlers[s3] = []);
        const i3 = this._escHandlers[s3];
        return i3.push(t3), { dispose: () => {
          const e4 = i3.indexOf(t3);
          -1 !== e4 && i3.splice(e4, 1);
        } };
      }
      clearEscHandler(e3) {
        this._escHandlers[this._identifier(e3, [48, 126])] && delete this._escHandlers[this._identifier(e3, [48, 126])];
      }
      setEscHandlerFallback(e3) {
        this._escHandlerFb = e3;
      }
      setExecuteHandler(e3, t3) {
        this._executeHandlers[e3.charCodeAt(0)] = t3;
      }
      clearExecuteHandler(e3) {
        this._executeHandlers[e3.charCodeAt(0)] && delete this._executeHandlers[e3.charCodeAt(0)];
      }
      setExecuteHandlerFallback(e3) {
        this._executeHandlerFb = e3;
      }
      registerCsiHandler(e3, t3) {
        const s3 = this._identifier(e3);
        void 0 === this._csiHandlers[s3] && (this._csiHandlers[s3] = []);
        const i3 = this._csiHandlers[s3];
        return i3.push(t3), { dispose: () => {
          const e4 = i3.indexOf(t3);
          -1 !== e4 && i3.splice(e4, 1);
        } };
      }
      clearCsiHandler(e3) {
        this._csiHandlers[this._identifier(e3)] && delete this._csiHandlers[this._identifier(e3)];
      }
      setCsiHandlerFallback(e3) {
        this._csiHandlerFb = e3;
      }
      registerDcsHandler(e3, t3) {
        return this._dcsParser.registerHandler(this._identifier(e3), t3);
      }
      clearDcsHandler(e3) {
        this._dcsParser.clearHandler(this._identifier(e3));
      }
      setDcsHandlerFallback(e3) {
        this._dcsParser.setHandlerFallback(e3);
      }
      registerOscHandler(e3, t3) {
        return this._oscParser.registerHandler(e3, t3);
      }
      clearOscHandler(e3) {
        this._oscParser.clearHandler(e3);
      }
      setOscHandlerFallback(e3) {
        this._oscParser.setHandlerFallback(e3);
      }
      setErrorHandler(e3) {
        this._errorHandler = e3;
      }
      clearErrorHandler() {
        this._errorHandler = this._errorHandlerFb;
      }
      reset() {
        this.currentState = this.initialState, this._oscParser.reset(), this._dcsParser.reset(), this._params.reset(), this._params.addParam(0), this._collect = 0, this.precedingJoinState = 0, 0 !== this._parseStack.state && (this._parseStack.state = 2, this._parseStack.handlers = []);
      }
      _preserveStack(e3, t3, s3, i3, r3) {
        this._parseStack.state = e3, this._parseStack.handlers = t3, this._parseStack.handlerPos = s3, this._parseStack.transition = i3, this._parseStack.chunkPos = r3;
      }
      parse(e3, t3, s3) {
        let i3, r3 = 0, n3 = 0, o2 = 0;
        if (this._parseStack.state) if (2 === this._parseStack.state) this._parseStack.state = 0, o2 = this._parseStack.chunkPos + 1;
        else {
          if (void 0 === s3 || 1 === this._parseStack.state) throw this._parseStack.state = 1, new Error("improper continuation due to previous async handler, giving up parsing");
          const t4 = this._parseStack.handlers;
          let n4 = this._parseStack.handlerPos - 1;
          switch (this._parseStack.state) {
            case 3:
              if (false === s3 && n4 > -1) {
                for (; n4 >= 0 && (i3 = t4[n4](this._params), true !== i3); n4--) if (i3 instanceof Promise) return this._parseStack.handlerPos = n4, i3;
              }
              this._parseStack.handlers = [];
              break;
            case 4:
              if (false === s3 && n4 > -1) {
                for (; n4 >= 0 && (i3 = t4[n4](), true !== i3); n4--) if (i3 instanceof Promise) return this._parseStack.handlerPos = n4, i3;
              }
              this._parseStack.handlers = [];
              break;
            case 6:
              if (r3 = e3[this._parseStack.chunkPos], i3 = this._dcsParser.unhook(24 !== r3 && 26 !== r3, s3), i3) return i3;
              27 === r3 && (this._parseStack.transition |= 1), this._params.reset(), this._params.addParam(0), this._collect = 0;
              break;
            case 5:
              if (r3 = e3[this._parseStack.chunkPos], i3 = this._oscParser.end(24 !== r3 && 26 !== r3, s3), i3) return i3;
              27 === r3 && (this._parseStack.transition |= 1), this._params.reset(), this._params.addParam(0), this._collect = 0;
          }
          this._parseStack.state = 0, o2 = this._parseStack.chunkPos + 1, this.precedingJoinState = 0, this.currentState = 15 & this._parseStack.transition;
        }
        for (let s4 = o2; s4 < t3; ++s4) {
          switch (r3 = e3[s4], n3 = this._transitions.table[this.currentState << 8 | (r3 < 160 ? r3 : h)], n3 >> 4) {
            case 2:
              for (let i4 = s4 + 1; ; ++i4) {
                if (i4 >= t3 || (r3 = e3[i4]) < 32 || r3 > 126 && r3 < h) {
                  this._printHandler(e3, s4, i4), s4 = i4 - 1;
                  break;
                }
                if (++i4 >= t3 || (r3 = e3[i4]) < 32 || r3 > 126 && r3 < h) {
                  this._printHandler(e3, s4, i4), s4 = i4 - 1;
                  break;
                }
                if (++i4 >= t3 || (r3 = e3[i4]) < 32 || r3 > 126 && r3 < h) {
                  this._printHandler(e3, s4, i4), s4 = i4 - 1;
                  break;
                }
                if (++i4 >= t3 || (r3 = e3[i4]) < 32 || r3 > 126 && r3 < h) {
                  this._printHandler(e3, s4, i4), s4 = i4 - 1;
                  break;
                }
              }
              break;
            case 3:
              this._executeHandlers[r3] ? this._executeHandlers[r3]() : this._executeHandlerFb(r3), this.precedingJoinState = 0;
              break;
            case 0:
              break;
            case 1:
              if (this._errorHandler({ position: s4, code: r3, currentState: this.currentState, collect: this._collect, params: this._params, abort: false }).abort) return;
              break;
            case 7:
              const o3 = this._csiHandlers[this._collect << 8 | r3];
              let a2 = o3 ? o3.length - 1 : -1;
              for (; a2 >= 0 && (i3 = o3[a2](this._params), true !== i3); a2--) if (i3 instanceof Promise) return this._preserveStack(3, o3, a2, n3, s4), i3;
              a2 < 0 && this._csiHandlerFb(this._collect << 8 | r3, this._params), this.precedingJoinState = 0;
              break;
            case 8:
              do {
                switch (r3) {
                  case 59:
                    this._params.addParam(0);
                    break;
                  case 58:
                    this._params.addSubParam(-1);
                    break;
                  default:
                    this._params.addDigit(r3 - 48);
                }
              } while (++s4 < t3 && (r3 = e3[s4]) > 47 && r3 < 60);
              s4--;
              break;
            case 9:
              this._collect <<= 8, this._collect |= r3;
              break;
            case 10:
              const c2 = this._escHandlers[this._collect << 8 | r3];
              let l = c2 ? c2.length - 1 : -1;
              for (; l >= 0 && (i3 = c2[l](), true !== i3); l--) if (i3 instanceof Promise) return this._preserveStack(4, c2, l, n3, s4), i3;
              l < 0 && this._escHandlerFb(this._collect << 8 | r3), this.precedingJoinState = 0;
              break;
            case 11:
              this._params.reset(), this._params.addParam(0), this._collect = 0;
              break;
            case 12:
              this._dcsParser.hook(this._collect << 8 | r3, this._params);
              break;
            case 13:
              for (let i4 = s4 + 1; ; ++i4) if (i4 >= t3 || 24 === (r3 = e3[i4]) || 26 === r3 || 27 === r3 || r3 > 127 && r3 < h) {
                this._dcsParser.put(e3, s4, i4), s4 = i4 - 1;
                break;
              }
              break;
            case 14:
              if (i3 = this._dcsParser.unhook(24 !== r3 && 26 !== r3), i3) return this._preserveStack(6, [], 0, n3, s4), i3;
              27 === r3 && (n3 |= 1), this._params.reset(), this._params.addParam(0), this._collect = 0, this.precedingJoinState = 0;
              break;
            case 4:
              this._oscParser.start();
              break;
            case 5:
              for (let i4 = s4 + 1; ; i4++) if (i4 >= t3 || (r3 = e3[i4]) < 32 || r3 > 127 && r3 < h) {
                this._oscParser.put(e3, s4, i4), s4 = i4 - 1;
                break;
              }
              break;
            case 6:
              if (i3 = this._oscParser.end(24 !== r3 && 26 !== r3), i3) return this._preserveStack(5, [], 0, n3, s4), i3;
              27 === r3 && (n3 |= 1), this._params.reset(), this._params.addParam(0), this._collect = 0, this.precedingJoinState = 0;
          }
          this.currentState = 15 & n3;
        }
      }
    }
    t2.EscapeSequenceParser = c;
  }, 1346: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.OscHandler = t2.OscParser = void 0;
    const i2 = s2(1263), r2 = s2(726), n2 = [];
    t2.OscParser = class {
      constructor() {
        this._state = 0, this._active = n2, this._id = -1, this._handlers = /* @__PURE__ */ Object.create(null), this._handlerFb = () => {
        }, this._stack = { paused: false, loopPosition: 0, fallThrough: false };
      }
      registerHandler(e3, t3) {
        void 0 === this._handlers[e3] && (this._handlers[e3] = []);
        const s3 = this._handlers[e3];
        return s3.push(t3), { dispose: () => {
          const e4 = s3.indexOf(t3);
          -1 !== e4 && s3.splice(e4, 1);
        } };
      }
      clearHandler(e3) {
        this._handlers[e3] && delete this._handlers[e3];
      }
      setHandlerFallback(e3) {
        this._handlerFb = e3;
      }
      dispose() {
        this._handlers = /* @__PURE__ */ Object.create(null), this._handlerFb = () => {
        }, this._active = n2;
      }
      reset() {
        if (2 === this._state) for (let e3 = this._stack.paused ? this._stack.loopPosition - 1 : this._active.length - 1; e3 >= 0; --e3) this._active[e3].end(false);
        this._stack.paused = false, this._active = n2, this._id = -1, this._state = 0;
      }
      _start() {
        if (this._active = this._handlers[this._id] || n2, this._active.length) for (let e3 = this._active.length - 1; e3 >= 0; e3--) this._active[e3].start();
        else this._handlerFb(this._id, "START");
      }
      _put(e3, t3, s3) {
        if (this._active.length) for (let i3 = this._active.length - 1; i3 >= 0; i3--) this._active[i3].put(e3, t3, s3);
        else this._handlerFb(this._id, "PUT", (0, r2.utf32ToString)(e3, t3, s3));
      }
      start() {
        this.reset(), this._state = 1;
      }
      put(e3, t3, s3) {
        if (3 !== this._state) {
          if (1 === this._state) for (; t3 < s3; ) {
            const s4 = e3[t3++];
            if (59 === s4) {
              this._state = 2, this._start();
              break;
            }
            if (s4 < 48 || 57 < s4) return void (this._state = 3);
            -1 === this._id && (this._id = 0), this._id = 10 * this._id + s4 - 48;
          }
          2 === this._state && s3 - t3 > 0 && this._put(e3, t3, s3);
        }
      }
      end(e3, t3 = true) {
        if (0 !== this._state) {
          if (3 !== this._state) if (1 === this._state && this._start(), this._active.length) {
            let s3 = false, i3 = this._active.length - 1, r3 = false;
            if (this._stack.paused && (i3 = this._stack.loopPosition - 1, s3 = t3, r3 = this._stack.fallThrough, this._stack.paused = false), !r3 && false === s3) {
              for (; i3 >= 0 && (s3 = this._active[i3].end(e3), true !== s3); i3--) if (s3 instanceof Promise) return this._stack.paused = true, this._stack.loopPosition = i3, this._stack.fallThrough = false, s3;
              i3--;
            }
            for (; i3 >= 0; i3--) if (s3 = this._active[i3].end(false), s3 instanceof Promise) return this._stack.paused = true, this._stack.loopPosition = i3, this._stack.fallThrough = true, s3;
          } else this._handlerFb(this._id, "END", e3);
          this._active = n2, this._id = -1, this._state = 0;
        }
      }
    }, t2.OscHandler = class {
      constructor(e3) {
        this._handler = e3, this._data = "", this._hitLimit = false;
      }
      start() {
        this._data = "", this._hitLimit = false;
      }
      put(e3, t3, s3) {
        this._hitLimit || (this._data += (0, r2.utf32ToString)(e3, t3, s3), this._data.length > i2.PAYLOAD_LIMIT && (this._data = "", this._hitLimit = true));
      }
      end(e3) {
        let t3 = false;
        if (this._hitLimit) t3 = false;
        else if (e3 && (t3 = this._handler(this._data), t3 instanceof Promise)) return t3.then(((e4) => (this._data = "", this._hitLimit = false, e4)));
        return this._data = "", this._hitLimit = false, t3;
      }
    };
  }, 7262: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.Params = void 0;
    const s2 = 2147483647;
    class i2 {
      static fromArray(e3) {
        const t3 = new i2();
        if (!e3.length) return t3;
        for (let s3 = Array.isArray(e3[0]) ? 1 : 0; s3 < e3.length; ++s3) {
          const i3 = e3[s3];
          if (Array.isArray(i3)) for (let e4 = 0; e4 < i3.length; ++e4) t3.addSubParam(i3[e4]);
          else t3.addParam(i3);
        }
        return t3;
      }
      constructor(e3 = 32, t3 = 32) {
        if (this.maxLength = e3, this.maxSubParamsLength = t3, t3 > 256) throw new Error("maxSubParamsLength must not be greater than 256");
        this.params = new Int32Array(e3), this.length = 0, this._subParams = new Int32Array(t3), this._subParamsLength = 0, this._subParamsIdx = new Uint16Array(e3), this._rejectDigits = false, this._rejectSubDigits = false, this._digitIsSub = false;
      }
      clone() {
        const e3 = new i2(this.maxLength, this.maxSubParamsLength);
        return e3.params.set(this.params), e3.length = this.length, e3._subParams.set(this._subParams), e3._subParamsLength = this._subParamsLength, e3._subParamsIdx.set(this._subParamsIdx), e3._rejectDigits = this._rejectDigits, e3._rejectSubDigits = this._rejectSubDigits, e3._digitIsSub = this._digitIsSub, e3;
      }
      toArray() {
        const e3 = [];
        for (let t3 = 0; t3 < this.length; ++t3) {
          e3.push(this.params[t3]);
          const s3 = this._subParamsIdx[t3] >> 8, i3 = 255 & this._subParamsIdx[t3];
          i3 - s3 > 0 && e3.push(Array.prototype.slice.call(this._subParams, s3, i3));
        }
        return e3;
      }
      reset() {
        this.length = 0, this._subParamsLength = 0, this._rejectDigits = false, this._rejectSubDigits = false, this._digitIsSub = false;
      }
      addParam(e3) {
        if (this._digitIsSub = false, this.length >= this.maxLength) this._rejectDigits = true;
        else {
          if (e3 < -1) throw new Error("values lesser than -1 are not allowed");
          this._subParamsIdx[this.length] = this._subParamsLength << 8 | this._subParamsLength, this.params[this.length++] = e3 > s2 ? s2 : e3;
        }
      }
      addSubParam(e3) {
        if (this._digitIsSub = true, this.length) if (this._rejectDigits || this._subParamsLength >= this.maxSubParamsLength) this._rejectSubDigits = true;
        else {
          if (e3 < -1) throw new Error("values lesser than -1 are not allowed");
          this._subParams[this._subParamsLength++] = e3 > s2 ? s2 : e3, this._subParamsIdx[this.length - 1]++;
        }
      }
      hasSubParams(e3) {
        return (255 & this._subParamsIdx[e3]) - (this._subParamsIdx[e3] >> 8) > 0;
      }
      getSubParams(e3) {
        const t3 = this._subParamsIdx[e3] >> 8, s3 = 255 & this._subParamsIdx[e3];
        return s3 - t3 > 0 ? this._subParams.subarray(t3, s3) : null;
      }
      getSubParamsAll() {
        const e3 = {};
        for (let t3 = 0; t3 < this.length; ++t3) {
          const s3 = this._subParamsIdx[t3] >> 8, i3 = 255 & this._subParamsIdx[t3];
          i3 - s3 > 0 && (e3[t3] = this._subParams.slice(s3, i3));
        }
        return e3;
      }
      addDigit(e3) {
        let t3;
        if (this._rejectDigits || !(t3 = this._digitIsSub ? this._subParamsLength : this.length) || this._digitIsSub && this._rejectSubDigits) return;
        const i3 = this._digitIsSub ? this._subParams : this.params, r2 = i3[t3 - 1];
        i3[t3 - 1] = ~r2 ? Math.min(10 * r2 + e3, s2) : e3;
      }
    }
    t2.Params = i2;
  }, 3027: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.AddonManager = void 0, t2.AddonManager = class {
      constructor() {
        this._addons = [];
      }
      dispose() {
        for (let e3 = this._addons.length - 1; e3 >= 0; e3--) this._addons[e3].instance.dispose();
      }
      loadAddon(e3, t3) {
        const s2 = { instance: t3, dispose: t3.dispose, isDisposed: false };
        this._addons.push(s2), t3.dispose = () => this._wrappedAddonDispose(s2), t3.activate(e3);
      }
      _wrappedAddonDispose(e3) {
        if (e3.isDisposed) return;
        let t3 = -1;
        for (let s2 = 0; s2 < this._addons.length; s2++) if (this._addons[s2] === e3) {
          t3 = s2;
          break;
        }
        if (-1 === t3) throw new Error("Could not dispose an addon that has not been loaded");
        e3.isDisposed = true, e3.dispose.apply(e3.instance), this._addons.splice(t3, 1);
      }
    };
  }, 3235: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.BufferApiView = void 0;
    const i2 = s2(793), r2 = s2(3055);
    t2.BufferApiView = class {
      constructor(e3, t3) {
        this._buffer = e3, this.type = t3;
      }
      init(e3) {
        return this._buffer = e3, this;
      }
      get cursorY() {
        return this._buffer.y;
      }
      get cursorX() {
        return this._buffer.x;
      }
      get viewportY() {
        return this._buffer.ydisp;
      }
      get baseY() {
        return this._buffer.ybase;
      }
      get length() {
        return this._buffer.lines.length;
      }
      getLine(e3) {
        const t3 = this._buffer.lines.get(e3);
        if (t3) return new i2.BufferLineApiView(t3);
      }
      getNullCell() {
        return new r2.CellData();
      }
    };
  }, 793: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.BufferLineApiView = void 0;
    const i2 = s2(3055);
    t2.BufferLineApiView = class {
      constructor(e3) {
        this._line = e3;
      }
      get isWrapped() {
        return this._line.isWrapped;
      }
      get length() {
        return this._line.length;
      }
      getCell(e3, t3) {
        if (!(e3 < 0 || e3 >= this._line.length)) return t3 ? (this._line.loadCell(e3, t3), t3) : this._line.loadCell(e3, new i2.CellData());
      }
      translateToString(e3, t3, s3) {
        return this._line.translateToString(e3, t3, s3);
      }
    };
  }, 5101: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.BufferNamespaceApi = void 0;
    const i2 = s2(3235), r2 = s2(7150), n2 = s2(802);
    class o extends r2.Disposable {
      constructor(e3) {
        super(), this._core = e3, this._onBufferChange = this._register(new n2.Emitter()), this.onBufferChange = this._onBufferChange.event, this._normal = new i2.BufferApiView(this._core.buffers.normal, "normal"), this._alternate = new i2.BufferApiView(this._core.buffers.alt, "alternate"), this._core.buffers.onBufferActivate((() => this._onBufferChange.fire(this.active)));
      }
      get active() {
        if (this._core.buffers.active === this._core.buffers.normal) return this.normal;
        if (this._core.buffers.active === this._core.buffers.alt) return this.alternate;
        throw new Error("Active buffer is neither normal nor alternate");
      }
      get normal() {
        return this._normal.init(this._core.buffers.normal);
      }
      get alternate() {
        return this._alternate.init(this._core.buffers.alt);
      }
    }
    t2.BufferNamespaceApi = o;
  }, 6097: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.ParserApi = void 0, t2.ParserApi = class {
      constructor(e3) {
        this._core = e3;
      }
      registerCsiHandler(e3, t3) {
        return this._core.registerCsiHandler(e3, ((e4) => t3(e4.toArray())));
      }
      addCsiHandler(e3, t3) {
        return this.registerCsiHandler(e3, t3);
      }
      registerDcsHandler(e3, t3) {
        return this._core.registerDcsHandler(e3, ((e4, s2) => t3(e4, s2.toArray())));
      }
      addDcsHandler(e3, t3) {
        return this.registerDcsHandler(e3, t3);
      }
      registerEscHandler(e3, t3) {
        return this._core.registerEscHandler(e3, t3);
      }
      addEscHandler(e3, t3) {
        return this.registerEscHandler(e3, t3);
      }
      registerOscHandler(e3, t3) {
        return this._core.registerOscHandler(e3, t3);
      }
      addOscHandler(e3, t3) {
        return this.registerOscHandler(e3, t3);
      }
    };
  }, 4335: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.UnicodeApi = void 0, t2.UnicodeApi = class {
      constructor(e3) {
        this._core = e3;
      }
      register(e3) {
        this._core.unicodeService.register(e3);
      }
      get versions() {
        return this._core.unicodeService.versions;
      }
      get activeVersion() {
        return this._core.unicodeService.activeVersion;
      }
      set activeVersion(e3) {
        this._core.unicodeService.activeVersion = e3;
      }
    };
  }, 9640: function(e2, t2, s2) {
    var i2 = this && this.__decorate || function(e3, t3, s3, i3) {
      var r3, n3 = arguments.length, o2 = n3 < 3 ? t3 : null === i3 ? i3 = Object.getOwnPropertyDescriptor(t3, s3) : i3;
      if ("object" == typeof Reflect && "function" == typeof Reflect.decorate) o2 = Reflect.decorate(e3, t3, s3, i3);
      else for (var a2 = e3.length - 1; a2 >= 0; a2--) (r3 = e3[a2]) && (o2 = (n3 < 3 ? r3(o2) : n3 > 3 ? r3(t3, s3, o2) : r3(t3, s3)) || o2);
      return n3 > 3 && o2 && Object.defineProperty(t3, s3, o2), o2;
    }, r2 = this && this.__param || function(e3, t3) {
      return function(s3, i3) {
        t3(s3, i3, e3);
      };
    };
    Object.defineProperty(t2, "__esModule", { value: true }), t2.BufferService = t2.MINIMUM_ROWS = t2.MINIMUM_COLS = void 0;
    const n2 = s2(7150), o = s2(4097), a = s2(6501), h = s2(802);
    t2.MINIMUM_COLS = 2, t2.MINIMUM_ROWS = 1;
    let c = class extends n2.Disposable {
      get buffer() {
        return this.buffers.active;
      }
      constructor(e3) {
        super(), this.isUserScrolling = false, this._onResize = this._register(new h.Emitter()), this.onResize = this._onResize.event, this._onScroll = this._register(new h.Emitter()), this.onScroll = this._onScroll.event, this.cols = Math.max(e3.rawOptions.cols || 0, t2.MINIMUM_COLS), this.rows = Math.max(e3.rawOptions.rows || 0, t2.MINIMUM_ROWS), this.buffers = this._register(new o.BufferSet(e3, this)), this._register(this.buffers.onBufferActivate(((e4) => {
          this._onScroll.fire(e4.activeBuffer.ydisp);
        })));
      }
      resize(e3, t3) {
        const s3 = this.cols !== e3, i3 = this.rows !== t3;
        this.cols = e3, this.rows = t3, this.buffers.resize(e3, t3), this._onResize.fire({ cols: e3, rows: t3, colsChanged: s3, rowsChanged: i3 });
      }
      reset() {
        this.buffers.reset(), this.isUserScrolling = false;
      }
      scroll(e3, t3 = false) {
        const s3 = this.buffer;
        let i3;
        i3 = this._cachedBlankLine, i3 && i3.length === this.cols && i3.getFg(0) === e3.fg && i3.getBg(0) === e3.bg || (i3 = s3.getBlankLine(e3, t3), this._cachedBlankLine = i3), i3.isWrapped = t3;
        const r3 = s3.ybase + s3.scrollTop, n3 = s3.ybase + s3.scrollBottom;
        if (0 === s3.scrollTop) {
          const e4 = s3.lines.isFull;
          n3 === s3.lines.length - 1 ? e4 ? s3.lines.recycle().copyFrom(i3) : s3.lines.push(i3.clone()) : s3.lines.splice(n3 + 1, 0, i3.clone()), e4 ? this.isUserScrolling && (s3.ydisp = Math.max(s3.ydisp - 1, 0)) : (s3.ybase++, this.isUserScrolling || s3.ydisp++);
        } else {
          const e4 = n3 - r3 + 1;
          s3.lines.shiftElements(r3 + 1, e4 - 1, -1), s3.lines.set(n3, i3.clone());
        }
        this.isUserScrolling || (s3.ydisp = s3.ybase), this._onScroll.fire(s3.ydisp);
      }
      scrollLines(e3, t3) {
        const s3 = this.buffer;
        if (e3 < 0) {
          if (0 === s3.ydisp) return;
          this.isUserScrolling = true;
        } else e3 + s3.ydisp >= s3.ybase && (this.isUserScrolling = false);
        const i3 = s3.ydisp;
        s3.ydisp = Math.max(Math.min(s3.ydisp + e3, s3.ybase), 0), i3 !== s3.ydisp && (t3 || this._onScroll.fire(s3.ydisp));
      }
    };
    t2.BufferService = c, t2.BufferService = c = i2([r2(0, a.IOptionsService)], c);
  }, 5746: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.CharsetService = void 0, t2.CharsetService = class {
      constructor() {
        this.glevel = 0, this._charsets = [];
      }
      reset() {
        this.charset = void 0, this._charsets = [], this.glevel = 0;
      }
      setgLevel(e3) {
        this.glevel = e3, this.charset = this._charsets[e3];
      }
      setgCharset(e3, t3) {
        this._charsets[e3] = t3, this.glevel === e3 && (this.charset = t3);
      }
    };
  }, 7792: function(e2, t2, s2) {
    var i2 = this && this.__decorate || function(e3, t3, s3, i3) {
      var r3, n3 = arguments.length, o2 = n3 < 3 ? t3 : null === i3 ? i3 = Object.getOwnPropertyDescriptor(t3, s3) : i3;
      if ("object" == typeof Reflect && "function" == typeof Reflect.decorate) o2 = Reflect.decorate(e3, t3, s3, i3);
      else for (var a2 = e3.length - 1; a2 >= 0; a2--) (r3 = e3[a2]) && (o2 = (n3 < 3 ? r3(o2) : n3 > 3 ? r3(t3, s3, o2) : r3(t3, s3)) || o2);
      return n3 > 3 && o2 && Object.defineProperty(t3, s3, o2), o2;
    }, r2 = this && this.__param || function(e3, t3) {
      return function(s3, i3) {
        t3(s3, i3, e3);
      };
    };
    Object.defineProperty(t2, "__esModule", { value: true }), t2.CoreMouseService = void 0;
    const n2 = s2(6501), o = s2(7150), a = s2(802), h = { NONE: { events: 0, restrict: () => false }, X10: { events: 1, restrict: (e3) => 4 !== e3.button && 1 === e3.action && (e3.ctrl = false, e3.alt = false, e3.shift = false, true) }, VT200: { events: 19, restrict: (e3) => 32 !== e3.action }, DRAG: { events: 23, restrict: (e3) => 32 !== e3.action || 3 !== e3.button }, ANY: { events: 31, restrict: (e3) => true } };
    function c(e3, t3) {
      let s3 = (e3.ctrl ? 16 : 0) | (e3.shift ? 4 : 0) | (e3.alt ? 8 : 0);
      return 4 === e3.button ? (s3 |= 64, s3 |= e3.action) : (s3 |= 3 & e3.button, 4 & e3.button && (s3 |= 64), 8 & e3.button && (s3 |= 128), 32 === e3.action ? s3 |= 32 : 0 !== e3.action || t3 || (s3 |= 3)), s3;
    }
    const l = String.fromCharCode, u = { DEFAULT: (e3) => {
      const t3 = [c(e3, false) + 32, e3.col + 32, e3.row + 32];
      return t3[0] > 255 || t3[1] > 255 || t3[2] > 255 ? "" : `\x1B[M${l(t3[0])}${l(t3[1])}${l(t3[2])}`;
    }, SGR: (e3) => {
      const t3 = 0 === e3.action && 4 !== e3.button ? "m" : "M";
      return `\x1B[<${c(e3, true)};${e3.col};${e3.row}${t3}`;
    }, SGR_PIXELS: (e3) => {
      const t3 = 0 === e3.action && 4 !== e3.button ? "m" : "M";
      return `\x1B[<${c(e3, true)};${e3.x};${e3.y}${t3}`;
    } };
    let d = class extends o.Disposable {
      constructor(e3, t3, s3) {
        super(), this._bufferService = e3, this._coreService = t3, this._optionsService = s3, this._protocols = {}, this._encodings = {}, this._activeProtocol = "", this._activeEncoding = "", this._lastEvent = null, this._wheelPartialScroll = 0, this._onProtocolChange = this._register(new a.Emitter()), this.onProtocolChange = this._onProtocolChange.event;
        for (const e4 of Object.keys(h)) this.addProtocol(e4, h[e4]);
        for (const e4 of Object.keys(u)) this.addEncoding(e4, u[e4]);
        this.reset();
      }
      addProtocol(e3, t3) {
        this._protocols[e3] = t3;
      }
      addEncoding(e3, t3) {
        this._encodings[e3] = t3;
      }
      get activeProtocol() {
        return this._activeProtocol;
      }
      get areMouseEventsActive() {
        return 0 !== this._protocols[this._activeProtocol].events;
      }
      set activeProtocol(e3) {
        if (!this._protocols[e3]) throw new Error(`unknown protocol "${e3}"`);
        this._activeProtocol = e3, this._onProtocolChange.fire(this._protocols[e3].events);
      }
      get activeEncoding() {
        return this._activeEncoding;
      }
      set activeEncoding(e3) {
        if (!this._encodings[e3]) throw new Error(`unknown encoding "${e3}"`);
        this._activeEncoding = e3;
      }
      reset() {
        this.activeProtocol = "NONE", this.activeEncoding = "DEFAULT", this._lastEvent = null, this._wheelPartialScroll = 0;
      }
      consumeWheelEvent(e3, t3, s3) {
        if (0 === e3.deltaY || e3.shiftKey) return 0;
        if (void 0 === t3 || void 0 === s3) return 0;
        const i3 = t3 / s3;
        let r3 = this._applyScrollModifier(e3.deltaY, e3);
        return e3.deltaMode === WheelEvent.DOM_DELTA_PIXEL ? (r3 /= i3 + 0, Math.abs(e3.deltaY) < 50 && (r3 *= 0.3), this._wheelPartialScroll += r3, r3 = Math.floor(Math.abs(this._wheelPartialScroll)) * (this._wheelPartialScroll > 0 ? 1 : -1), this._wheelPartialScroll %= 1) : e3.deltaMode === WheelEvent.DOM_DELTA_PAGE && (r3 *= this._bufferService.rows), r3;
      }
      _applyScrollModifier(e3, t3) {
        return t3.altKey || t3.ctrlKey || t3.shiftKey ? e3 * this._optionsService.rawOptions.fastScrollSensitivity * this._optionsService.rawOptions.scrollSensitivity : e3 * this._optionsService.rawOptions.scrollSensitivity;
      }
      triggerMouseEvent(e3) {
        if (e3.col < 0 || e3.col >= this._bufferService.cols || e3.row < 0 || e3.row >= this._bufferService.rows) return false;
        if (4 === e3.button && 32 === e3.action) return false;
        if (3 === e3.button && 32 !== e3.action) return false;
        if (4 !== e3.button && (2 === e3.action || 3 === e3.action)) return false;
        if (e3.col++, e3.row++, 32 === e3.action && this._lastEvent && this._equalEvents(this._lastEvent, e3, "SGR_PIXELS" === this._activeEncoding)) return false;
        if (!this._protocols[this._activeProtocol].restrict(e3)) return false;
        const t3 = this._encodings[this._activeEncoding](e3);
        return t3 && ("DEFAULT" === this._activeEncoding ? this._coreService.triggerBinaryEvent(t3) : this._coreService.triggerDataEvent(t3, true)), this._lastEvent = e3, true;
      }
      explainEvents(e3) {
        return { down: !!(1 & e3), up: !!(2 & e3), drag: !!(4 & e3), move: !!(8 & e3), wheel: !!(16 & e3) };
      }
      _equalEvents(e3, t3, s3) {
        if (s3) {
          if (e3.x !== t3.x) return false;
          if (e3.y !== t3.y) return false;
        } else {
          if (e3.col !== t3.col) return false;
          if (e3.row !== t3.row) return false;
        }
        return e3.button === t3.button && e3.action === t3.action && e3.ctrl === t3.ctrl && e3.alt === t3.alt && e3.shift === t3.shift;
      }
    };
    t2.CoreMouseService = d, t2.CoreMouseService = d = i2([r2(0, n2.IBufferService), r2(1, n2.ICoreService), r2(2, n2.IOptionsService)], d);
  }, 4071: function(e2, t2, s2) {
    var i2 = this && this.__decorate || function(e3, t3, s3, i3) {
      var r3, n3 = arguments.length, o2 = n3 < 3 ? t3 : null === i3 ? i3 = Object.getOwnPropertyDescriptor(t3, s3) : i3;
      if ("object" == typeof Reflect && "function" == typeof Reflect.decorate) o2 = Reflect.decorate(e3, t3, s3, i3);
      else for (var a2 = e3.length - 1; a2 >= 0; a2--) (r3 = e3[a2]) && (o2 = (n3 < 3 ? r3(o2) : n3 > 3 ? r3(t3, s3, o2) : r3(t3, s3)) || o2);
      return n3 > 3 && o2 && Object.defineProperty(t3, s3, o2), o2;
    }, r2 = this && this.__param || function(e3, t3) {
      return function(s3, i3) {
        t3(s3, i3, e3);
      };
    };
    Object.defineProperty(t2, "__esModule", { value: true }), t2.CoreService = void 0;
    const n2 = s2(7453), o = s2(7150), a = s2(6501), h = s2(802), c = Object.freeze({ insertMode: false }), l = Object.freeze({ applicationCursorKeys: false, applicationKeypad: false, bracketedPasteMode: false, cursorBlink: void 0, cursorStyle: void 0, origin: false, reverseWraparound: false, sendFocus: false, synchronizedOutput: false, wraparound: true });
    let u = class extends o.Disposable {
      constructor(e3, t3, s3) {
        super(), this._bufferService = e3, this._logService = t3, this._optionsService = s3, this.isCursorInitialized = false, this.isCursorHidden = false, this._onData = this._register(new h.Emitter()), this.onData = this._onData.event, this._onUserInput = this._register(new h.Emitter()), this.onUserInput = this._onUserInput.event, this._onBinary = this._register(new h.Emitter()), this.onBinary = this._onBinary.event, this._onRequestScrollToBottom = this._register(new h.Emitter()), this.onRequestScrollToBottom = this._onRequestScrollToBottom.event, this.modes = (0, n2.clone)(c), this.decPrivateModes = (0, n2.clone)(l);
      }
      reset() {
        this.modes = (0, n2.clone)(c), this.decPrivateModes = (0, n2.clone)(l);
      }
      triggerDataEvent(e3, t3 = false) {
        if (this._optionsService.rawOptions.disableStdin) return;
        const s3 = this._bufferService.buffer;
        t3 && this._optionsService.rawOptions.scrollOnUserInput && s3.ybase !== s3.ydisp && this._onRequestScrollToBottom.fire(), t3 && this._onUserInput.fire(), this._logService.debug(`sending data "${e3}"`), this._logService.trace("sending data (codes)", (() => e3.split("").map(((e4) => e4.charCodeAt(0))))), this._onData.fire(e3);
      }
      triggerBinaryEvent(e3) {
        this._optionsService.rawOptions.disableStdin || (this._logService.debug(`sending binary "${e3}"`), this._logService.trace("sending binary (codes)", (() => e3.split("").map(((e4) => e4.charCodeAt(0))))), this._onBinary.fire(e3));
      }
    };
    t2.CoreService = u, t2.CoreService = u = i2([r2(0, a.IBufferService), r2(1, a.ILogService), r2(2, a.IOptionsService)], u);
  }, 6025: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.InstantiationService = t2.ServiceCollection = void 0;
    const i2 = s2(6501), r2 = s2(6201);
    class n2 {
      constructor(...e3) {
        this._entries = /* @__PURE__ */ new Map();
        for (const [t3, s3] of e3) this.set(t3, s3);
      }
      set(e3, t3) {
        const s3 = this._entries.get(e3);
        return this._entries.set(e3, t3), s3;
      }
      forEach(e3) {
        for (const [t3, s3] of this._entries.entries()) e3(t3, s3);
      }
      has(e3) {
        return this._entries.has(e3);
      }
      get(e3) {
        return this._entries.get(e3);
      }
    }
    t2.ServiceCollection = n2, t2.InstantiationService = class {
      constructor() {
        this._services = new n2(), this._services.set(i2.IInstantiationService, this);
      }
      setService(e3, t3) {
        this._services.set(e3, t3);
      }
      getService(e3) {
        return this._services.get(e3);
      }
      createInstance(e3, ...t3) {
        const s3 = (0, r2.getServiceDependencies)(e3).sort(((e4, t4) => e4.index - t4.index)), i3 = [];
        for (const t4 of s3) {
          const s4 = this._services.get(t4.id);
          if (!s4) throw new Error(`[createInstance] ${e3.name} depends on UNKNOWN service ${t4.id._id}.`);
          i3.push(s4);
        }
        const n3 = s3.length > 0 ? s3[0].index : t3.length;
        if (t3.length !== n3) throw new Error(`[createInstance] First service dependency of ${e3.name} at position ${n3 + 1} conflicts with ${t3.length} static arguments`);
        return new e3(...[...t3, ...i3]);
      }
    };
  }, 7276: function(e2, t2, s2) {
    var i2 = this && this.__decorate || function(e3, t3, s3, i3) {
      var r3, n3 = arguments.length, o2 = n3 < 3 ? t3 : null === i3 ? i3 = Object.getOwnPropertyDescriptor(t3, s3) : i3;
      if ("object" == typeof Reflect && "function" == typeof Reflect.decorate) o2 = Reflect.decorate(e3, t3, s3, i3);
      else for (var a2 = e3.length - 1; a2 >= 0; a2--) (r3 = e3[a2]) && (o2 = (n3 < 3 ? r3(o2) : n3 > 3 ? r3(t3, s3, o2) : r3(t3, s3)) || o2);
      return n3 > 3 && o2 && Object.defineProperty(t3, s3, o2), o2;
    }, r2 = this && this.__param || function(e3, t3) {
      return function(s3, i3) {
        t3(s3, i3, e3);
      };
    };
    Object.defineProperty(t2, "__esModule", { value: true }), t2.LogService = void 0, t2.setTraceLogger = function(e3) {
      h = e3;
    }, t2.traceCall = function(e3, t3, s3) {
      if ("function" != typeof s3.value) throw new Error("not supported");
      const i3 = s3.value;
      s3.value = function(...e4) {
        if (h.logLevel !== o.LogLevelEnum.TRACE) return i3.apply(this, e4);
        h.trace(`GlyphRenderer#${i3.name}(${e4.map(((e5) => JSON.stringify(e5))).join(", ")})`);
        const t4 = i3.apply(this, e4);
        return h.trace(`GlyphRenderer#${i3.name} return`, t4), t4;
      };
    };
    const n2 = s2(7150), o = s2(6501), a = { trace: o.LogLevelEnum.TRACE, debug: o.LogLevelEnum.DEBUG, info: o.LogLevelEnum.INFO, warn: o.LogLevelEnum.WARN, error: o.LogLevelEnum.ERROR, off: o.LogLevelEnum.OFF };
    let h, c = class extends n2.Disposable {
      get logLevel() {
        return this._logLevel;
      }
      constructor(e3) {
        super(), this._optionsService = e3, this._logLevel = o.LogLevelEnum.OFF, this._updateLogLevel(), this._register(this._optionsService.onSpecificOptionChange("logLevel", (() => this._updateLogLevel()))), h = this;
      }
      _updateLogLevel() {
        this._logLevel = a[this._optionsService.rawOptions.logLevel];
      }
      _evalLazyOptionalParams(e3) {
        for (let t3 = 0; t3 < e3.length; t3++) "function" == typeof e3[t3] && (e3[t3] = e3[t3]());
      }
      _log(e3, t3, s3) {
        this._evalLazyOptionalParams(s3), e3.call(console, (this._optionsService.options.logger ? "" : "xterm.js: ") + t3, ...s3);
      }
      trace(e3, ...t3) {
        var _a, _b;
        this._logLevel <= o.LogLevelEnum.TRACE && this._log((_b = (_a = this._optionsService.options.logger) == null ? void 0 : _a.trace.bind(this._optionsService.options.logger)) != null ? _b : console.log, e3, t3);
      }
      debug(e3, ...t3) {
        var _a, _b;
        this._logLevel <= o.LogLevelEnum.DEBUG && this._log((_b = (_a = this._optionsService.options.logger) == null ? void 0 : _a.debug.bind(this._optionsService.options.logger)) != null ? _b : console.log, e3, t3);
      }
      info(e3, ...t3) {
        var _a, _b;
        this._logLevel <= o.LogLevelEnum.INFO && this._log((_b = (_a = this._optionsService.options.logger) == null ? void 0 : _a.info.bind(this._optionsService.options.logger)) != null ? _b : console.info, e3, t3);
      }
      warn(e3, ...t3) {
        var _a, _b;
        this._logLevel <= o.LogLevelEnum.WARN && this._log((_b = (_a = this._optionsService.options.logger) == null ? void 0 : _a.warn.bind(this._optionsService.options.logger)) != null ? _b : console.warn, e3, t3);
      }
      error(e3, ...t3) {
        var _a, _b;
        this._logLevel <= o.LogLevelEnum.ERROR && this._log((_b = (_a = this._optionsService.options.logger) == null ? void 0 : _a.error.bind(this._optionsService.options.logger)) != null ? _b : console.error, e3, t3);
      }
    };
    t2.LogService = c, t2.LogService = c = i2([r2(0, o.IOptionsService)], c);
  }, 56: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.OptionsService = t2.DEFAULT_OPTIONS = void 0;
    const i2 = s2(7150), r2 = s2(701), n2 = s2(802);
    t2.DEFAULT_OPTIONS = { cols: 80, rows: 24, cursorBlink: false, cursorStyle: "block", cursorWidth: 1, cursorInactiveStyle: "outline", customGlyphs: true, drawBoldTextInBrightColors: true, documentOverride: null, fastScrollModifier: "alt", fastScrollSensitivity: 5, fontFamily: "monospace", fontSize: 15, fontWeight: "normal", fontWeightBold: "bold", ignoreBracketedPasteMode: false, lineHeight: 1, letterSpacing: 0, linkHandler: null, logLevel: "info", logger: null, scrollback: 1e3, scrollOnEraseInDisplay: false, scrollOnUserInput: true, scrollSensitivity: 1, screenReaderMode: false, smoothScrollDuration: 0, macOptionIsMeta: false, macOptionClickForcesSelection: false, minimumContrastRatio: 1, disableStdin: false, allowProposedApi: false, allowTransparency: false, tabStopWidth: 8, theme: {}, reflowCursorLine: false, rescaleOverlappingGlyphs: false, rightClickSelectsWord: r2.isMac, windowOptions: {}, windowsMode: false, windowsPty: {}, wordSeparator: " ()[]{}',\"`", altClickMovesCursor: true, convertEol: false, termName: "xterm", cancelEvents: false, overviewRuler: {} };
    const o = ["normal", "bold", "100", "200", "300", "400", "500", "600", "700", "800", "900"];
    class a extends i2.Disposable {
      constructor(e3) {
        super(), this._onOptionChange = this._register(new n2.Emitter()), this.onOptionChange = this._onOptionChange.event;
        const s3 = __spreadValues({}, t2.DEFAULT_OPTIONS);
        for (const t3 in e3) if (t3 in s3) try {
          const i3 = e3[t3];
          s3[t3] = this._sanitizeAndValidateOption(t3, i3);
        } catch (e4) {
          console.error(e4);
        }
        this.rawOptions = s3, this.options = __spreadValues({}, s3), this._setupOptions(), this._register((0, i2.toDisposable)((() => {
          this.rawOptions.linkHandler = null, this.rawOptions.documentOverride = null;
        })));
      }
      onSpecificOptionChange(e3, t3) {
        return this.onOptionChange(((s3) => {
          s3 === e3 && t3(this.rawOptions[e3]);
        }));
      }
      onMultipleOptionChange(e3, t3) {
        return this.onOptionChange(((s3) => {
          -1 !== e3.indexOf(s3) && t3();
        }));
      }
      _setupOptions() {
        const e3 = (e4) => {
          if (!(e4 in t2.DEFAULT_OPTIONS)) throw new Error(`No option with key "${e4}"`);
          return this.rawOptions[e4];
        }, s3 = (e4, s4) => {
          if (!(e4 in t2.DEFAULT_OPTIONS)) throw new Error(`No option with key "${e4}"`);
          s4 = this._sanitizeAndValidateOption(e4, s4), this.rawOptions[e4] !== s4 && (this.rawOptions[e4] = s4, this._onOptionChange.fire(e4));
        };
        for (const t3 in this.rawOptions) {
          const i3 = { get: e3.bind(this, t3), set: s3.bind(this, t3) };
          Object.defineProperty(this.options, t3, i3);
        }
      }
      _sanitizeAndValidateOption(e3, s3) {
        switch (e3) {
          case "cursorStyle":
            if (s3 || (s3 = t2.DEFAULT_OPTIONS[e3]), !/* @__PURE__ */ (function(e4) {
              return "block" === e4 || "underline" === e4 || "bar" === e4;
            })(s3)) throw new Error(`"${s3}" is not a valid value for ${e3}`);
            break;
          case "wordSeparator":
            s3 || (s3 = t2.DEFAULT_OPTIONS[e3]);
            break;
          case "fontWeight":
          case "fontWeightBold":
            if ("number" == typeof s3 && 1 <= s3 && s3 <= 1e3) break;
            s3 = o.includes(s3) ? s3 : t2.DEFAULT_OPTIONS[e3];
            break;
          case "cursorWidth":
            s3 = Math.floor(s3);
          case "lineHeight":
          case "tabStopWidth":
            if (s3 < 1) throw new Error(`${e3} cannot be less than 1, value: ${s3}`);
            break;
          case "minimumContrastRatio":
            s3 = Math.max(1, Math.min(21, Math.round(10 * s3) / 10));
            break;
          case "scrollback":
            if ((s3 = Math.min(s3, 4294967295)) < 0) throw new Error(`${e3} cannot be less than 0, value: ${s3}`);
            break;
          case "fastScrollSensitivity":
          case "scrollSensitivity":
            if (s3 <= 0) throw new Error(`${e3} cannot be less than or equal to 0, value: ${s3}`);
            break;
          case "rows":
          case "cols":
            if (!s3 && 0 !== s3) throw new Error(`${e3} must be numeric, value: ${s3}`);
            break;
          case "windowsPty":
            s3 = s3 != null ? s3 : {};
        }
        return s3;
      }
    }
    t2.OptionsService = a;
  }, 8811: function(e2, t2, s2) {
    var i2 = this && this.__decorate || function(e3, t3, s3, i3) {
      var r3, n3 = arguments.length, o2 = n3 < 3 ? t3 : null === i3 ? i3 = Object.getOwnPropertyDescriptor(t3, s3) : i3;
      if ("object" == typeof Reflect && "function" == typeof Reflect.decorate) o2 = Reflect.decorate(e3, t3, s3, i3);
      else for (var a = e3.length - 1; a >= 0; a--) (r3 = e3[a]) && (o2 = (n3 < 3 ? r3(o2) : n3 > 3 ? r3(t3, s3, o2) : r3(t3, s3)) || o2);
      return n3 > 3 && o2 && Object.defineProperty(t3, s3, o2), o2;
    }, r2 = this && this.__param || function(e3, t3) {
      return function(s3, i3) {
        t3(s3, i3, e3);
      };
    };
    Object.defineProperty(t2, "__esModule", { value: true }), t2.OscLinkService = void 0;
    const n2 = s2(6501);
    let o = class {
      constructor(e3) {
        this._bufferService = e3, this._nextId = 1, this._entriesWithId = /* @__PURE__ */ new Map(), this._dataByLinkId = /* @__PURE__ */ new Map();
      }
      registerLink(e3) {
        const t3 = this._bufferService.buffer;
        if (void 0 === e3.id) {
          const s4 = t3.addMarker(t3.ybase + t3.y), i4 = { data: e3, id: this._nextId++, lines: [s4] };
          return s4.onDispose((() => this._removeMarkerFromLink(i4, s4))), this._dataByLinkId.set(i4.id, i4), i4.id;
        }
        const s3 = e3, i3 = this._getEntryIdKey(s3), r3 = this._entriesWithId.get(i3);
        if (r3) return this.addLineToLink(r3.id, t3.ybase + t3.y), r3.id;
        const n3 = t3.addMarker(t3.ybase + t3.y), o2 = { id: this._nextId++, key: this._getEntryIdKey(s3), data: s3, lines: [n3] };
        return n3.onDispose((() => this._removeMarkerFromLink(o2, n3))), this._entriesWithId.set(o2.key, o2), this._dataByLinkId.set(o2.id, o2), o2.id;
      }
      addLineToLink(e3, t3) {
        const s3 = this._dataByLinkId.get(e3);
        if (s3 && s3.lines.every(((e4) => e4.line !== t3))) {
          const e4 = this._bufferService.buffer.addMarker(t3);
          s3.lines.push(e4), e4.onDispose((() => this._removeMarkerFromLink(s3, e4)));
        }
      }
      getLinkData(e3) {
        var _a;
        return (_a = this._dataByLinkId.get(e3)) == null ? void 0 : _a.data;
      }
      _getEntryIdKey(e3) {
        return `${e3.id};;${e3.uri}`;
      }
      _removeMarkerFromLink(e3, t3) {
        const s3 = e3.lines.indexOf(t3);
        -1 !== s3 && (e3.lines.splice(s3, 1), 0 === e3.lines.length && (void 0 !== e3.data.id && this._entriesWithId.delete(e3.key), this._dataByLinkId.delete(e3.id)));
      }
    };
    t2.OscLinkService = o, t2.OscLinkService = o = i2([r2(0, n2.IBufferService)], o);
  }, 6201: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.serviceRegistry = void 0, t2.getServiceDependencies = function(e3) {
      return e3[i2] || [];
    }, t2.createDecorator = function(e3) {
      if (t2.serviceRegistry.has(e3)) return t2.serviceRegistry.get(e3);
      const r2 = function(e4, t3, n2) {
        if (3 !== arguments.length) throw new Error("@IServiceName-decorator can only be used to decorate a parameter");
        !(function(e5, t4, r3) {
          t4[s2] === t4 ? t4[i2].push({ id: e5, index: r3 }) : (t4[i2] = [{ id: e5, index: r3 }], t4[s2] = t4);
        })(r2, e4, n2);
      };
      return r2._id = e3, t2.serviceRegistry.set(e3, r2), r2;
    };
    const s2 = "di$target", i2 = "di$dependencies";
    t2.serviceRegistry = /* @__PURE__ */ new Map();
  }, 6501: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.IDecorationService = t2.IUnicodeService = t2.IOscLinkService = t2.IOptionsService = t2.ILogService = t2.LogLevelEnum = t2.IInstantiationService = t2.ICharsetService = t2.ICoreService = t2.ICoreMouseService = t2.IBufferService = void 0;
    const i2 = s2(6201);
    var r2;
    t2.IBufferService = (0, i2.createDecorator)("BufferService"), t2.ICoreMouseService = (0, i2.createDecorator)("CoreMouseService"), t2.ICoreService = (0, i2.createDecorator)("CoreService"), t2.ICharsetService = (0, i2.createDecorator)("CharsetService"), t2.IInstantiationService = (0, i2.createDecorator)("InstantiationService"), (function(e3) {
      e3[e3.TRACE = 0] = "TRACE", e3[e3.DEBUG = 1] = "DEBUG", e3[e3.INFO = 2] = "INFO", e3[e3.WARN = 3] = "WARN", e3[e3.ERROR = 4] = "ERROR", e3[e3.OFF = 5] = "OFF";
    })(r2 || (t2.LogLevelEnum = r2 = {})), t2.ILogService = (0, i2.createDecorator)("LogService"), t2.IOptionsService = (0, i2.createDecorator)("OptionsService"), t2.IOscLinkService = (0, i2.createDecorator)("OscLinkService"), t2.IUnicodeService = (0, i2.createDecorator)("UnicodeService"), t2.IDecorationService = (0, i2.createDecorator)("DecorationService");
  }, 6415: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.UnicodeService = void 0;
    const i2 = s2(7428), r2 = s2(802);
    class n2 {
      static extractShouldJoin(e3) {
        return !!(1 & e3);
      }
      static extractWidth(e3) {
        return e3 >> 1 & 3;
      }
      static extractCharKind(e3) {
        return e3 >> 3;
      }
      static createPropertyValue(e3, t3, s3 = false) {
        return (16777215 & e3) << 3 | (3 & t3) << 1 | (s3 ? 1 : 0);
      }
      constructor() {
        this._providers = /* @__PURE__ */ Object.create(null), this._active = "", this._onChange = new r2.Emitter(), this.onChange = this._onChange.event;
        const e3 = new i2.UnicodeV6();
        this.register(e3), this._active = e3.version, this._activeProvider = e3;
      }
      dispose() {
        this._onChange.dispose();
      }
      get versions() {
        return Object.keys(this._providers);
      }
      get activeVersion() {
        return this._active;
      }
      set activeVersion(e3) {
        if (!this._providers[e3]) throw new Error(`unknown Unicode version "${e3}"`);
        this._active = e3, this._activeProvider = this._providers[e3], this._onChange.fire(e3);
      }
      register(e3) {
        this._providers[e3.version] = e3;
      }
      wcwidth(e3) {
        return this._activeProvider.wcwidth(e3);
      }
      getStringCellWidth(e3) {
        let t3 = 0, s3 = 0;
        const i3 = e3.length;
        for (let r3 = 0; r3 < i3; ++r3) {
          let o = e3.charCodeAt(r3);
          if (55296 <= o && o <= 56319) {
            if (++r3 >= i3) return t3 + this.wcwidth(o);
            const s4 = e3.charCodeAt(r3);
            56320 <= s4 && s4 <= 57343 ? o = 1024 * (o - 55296) + s4 - 56320 + 65536 : t3 += this.wcwidth(s4);
          }
          const a = this.charProperties(o, s3);
          let h = n2.extractWidth(a);
          n2.extractShouldJoin(a) && (h -= n2.extractWidth(s3)), t3 += h, s3 = a;
        }
        return t3;
      }
      charProperties(e3, t3) {
        return this._activeProvider.charProperties(e3, t3);
      }
    }
    t2.UnicodeService = n2;
  }, 5856: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.Terminal = void 0;
    const i2 = s2(6107), r2 = s2(5777), n2 = s2(802);
    class o extends r2.CoreTerminal {
      constructor(e3 = {}) {
        super(e3), this._onBell = this._register(new n2.Emitter()), this.onBell = this._onBell.event, this._onCursorMove = this._register(new n2.Emitter()), this.onCursorMove = this._onCursorMove.event, this._onTitleChange = this._register(new n2.Emitter()), this.onTitleChange = this._onTitleChange.event, this._onA11yCharEmitter = this._register(new n2.Emitter()), this.onA11yChar = this._onA11yCharEmitter.event, this._onA11yTabEmitter = this._register(new n2.Emitter()), this.onA11yTab = this._onA11yTabEmitter.event, this._setup(), this._register(this._inputHandler.onRequestBell((() => this.bell()))), this._register(this._inputHandler.onRequestReset((() => this.reset()))), this._register(n2.Event.forward(this._inputHandler.onCursorMove, this._onCursorMove)), this._register(n2.Event.forward(this._inputHandler.onTitleChange, this._onTitleChange)), this._register(n2.Event.forward(this._inputHandler.onA11yChar, this._onA11yCharEmitter)), this._register(n2.Event.forward(this._inputHandler.onA11yTab, this._onA11yTabEmitter));
      }
      get buffer() {
        return this.buffers.active;
      }
      get markers() {
        return this.buffer.markers;
      }
      addMarker(e3) {
        if (this.buffer === this.buffers.normal) return this.buffer.addMarker(this.buffer.ybase + this.buffer.y + e3);
      }
      bell() {
        this._onBell.fire();
      }
      input(e3, t3 = true) {
        this.coreService.triggerDataEvent(e3, t3);
      }
      resize(e3, t3) {
        e3 === this.cols && t3 === this.rows || super.resize(e3, t3);
      }
      clear() {
        if (0 !== this.buffer.ybase || 0 !== this.buffer.y) {
          this.buffer.lines.set(0, this.buffer.lines.get(this.buffer.ybase + this.buffer.y)), this.buffer.lines.length = 1, this.buffer.ydisp = 0, this.buffer.ybase = 0, this.buffer.y = 0;
          for (let e3 = 1; e3 < this.rows; e3++) this.buffer.lines.push(this.buffer.getBlankLine(i2.DEFAULT_ATTR_DATA));
          this._onScroll.fire({ position: this.buffer.ydisp });
        }
      }
      reset() {
        this.options.rows = this.rows, this.options.cols = this.cols, this._setup(), super.reset();
      }
    }
    t2.Terminal = o;
  }, 3058: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.Permutation = t2.CallbackIterable = t2.ArrayQueue = t2.booleanComparator = t2.numberComparator = t2.CompareResult = void 0, t2.tail = function(e3, t3 = 0) {
      return e3[e3.length - (1 + t3)];
    }, t2.tail2 = function(e3) {
      if (0 === e3.length) throw new Error("Invalid tail call");
      return [e3.slice(0, e3.length - 1), e3[e3.length - 1]];
    }, t2.equals = function(e3, t3, s3 = (e4, t4) => e4 === t4) {
      if (e3 === t3) return true;
      if (!e3 || !t3) return false;
      if (e3.length !== t3.length) return false;
      for (let i3 = 0, r3 = e3.length; i3 < r3; i3++) if (!s3(e3[i3], t3[i3])) return false;
      return true;
    }, t2.removeFastWithoutKeepingOrder = function(e3, t3) {
      const s3 = e3.length - 1;
      t3 < s3 && (e3[t3] = e3[s3]), e3.pop();
    }, t2.binarySearch = function(e3, t3, s3) {
      return n2(e3.length, ((i3) => s3(e3[i3], t3)));
    }, t2.binarySearch2 = n2, t2.quickSelect = function e3(t3, s3, i3) {
      if ((t3 |= 0) >= s3.length) throw new TypeError("invalid index");
      const r3 = s3[Math.floor(s3.length * Math.random())], n3 = [], o2 = [], a2 = [];
      for (const e4 of s3) {
        const t4 = i3(e4, r3);
        t4 < 0 ? n3.push(e4) : t4 > 0 ? o2.push(e4) : a2.push(e4);
      }
      return t3 < n3.length ? e3(t3, n3, i3) : t3 < n3.length + a2.length ? a2[0] : e3(t3 - (n3.length + a2.length), o2, i3);
    }, t2.groupBy = function(e3, t3) {
      const s3 = [];
      let i3;
      for (const r3 of e3.slice(0).sort(t3)) i3 && 0 === t3(i3[0], r3) ? i3.push(r3) : (i3 = [r3], s3.push(i3));
      return s3;
    }, t2.groupAdjacentBy = function* (e3, t3) {
      let s3, i3;
      for (const r3 of e3) void 0 !== i3 && t3(i3, r3) ? s3.push(r3) : (s3 && (yield s3), s3 = [r3]), i3 = r3;
      s3 && (yield s3);
    }, t2.forEachAdjacent = function(e3, t3) {
      for (let s3 = 0; s3 <= e3.length; s3++) t3(0 === s3 ? void 0 : e3[s3 - 1], s3 === e3.length ? void 0 : e3[s3]);
    }, t2.forEachWithNeighbors = function(e3, t3) {
      for (let s3 = 0; s3 < e3.length; s3++) t3(0 === s3 ? void 0 : e3[s3 - 1], e3[s3], s3 + 1 === e3.length ? void 0 : e3[s3 + 1]);
    }, t2.sortedDiff = o, t2.delta = function(e3, t3, s3) {
      const i3 = o(e3, t3, s3), r3 = [], n3 = [];
      for (const t4 of i3) r3.push(...e3.slice(t4.start, t4.start + t4.deleteCount)), n3.push(...t4.toInsert);
      return { removed: r3, added: n3 };
    }, t2.top = function(e3, t3, s3) {
      if (0 === s3) return [];
      const i3 = e3.slice(0, s3).sort(t3);
      return a(e3, t3, i3, s3, e3.length), i3;
    }, t2.topAsync = function(e3, t3, s3, r3, n3) {
      return 0 === s3 ? Promise.resolve([]) : new Promise(((o2, h2) => {
        (() => __async(null, null, function* () {
          const o3 = e3.length, h3 = e3.slice(0, s3).sort(t3);
          for (let c2 = s3, l2 = Math.min(s3 + r3, o3); c2 < o3; c2 = l2, l2 = Math.min(l2 + r3, o3)) {
            if (c2 > s3 && (yield new Promise(((e4) => setTimeout(e4)))), n3 && n3.isCancellationRequested) throw new i2.CancellationError();
            a(e3, t3, h3, c2, l2);
          }
          return h3;
        }))().then(o2, h2);
      }));
    }, t2.coalesce = function(e3) {
      return e3.filter(((e4) => !!e4));
    }, t2.coalesceInPlace = function(e3) {
      let t3 = 0;
      for (let s3 = 0; s3 < e3.length; s3++) e3[s3] && (e3[t3] = e3[s3], t3 += 1);
      e3.length = t3;
    }, t2.move = function(e3, t3, s3) {
      e3.splice(s3, 0, e3.splice(t3, 1)[0]);
    }, t2.isFalsyOrEmpty = function(e3) {
      return !Array.isArray(e3) || 0 === e3.length;
    }, t2.isNonEmptyArray = function(e3) {
      return Array.isArray(e3) && e3.length > 0;
    }, t2.distinct = function(e3, t3 = (e4) => e4) {
      const s3 = /* @__PURE__ */ new Set();
      return e3.filter(((e4) => {
        const i3 = t3(e4);
        return !s3.has(i3) && (s3.add(i3), true);
      }));
    }, t2.uniqueFilter = function(e3) {
      const t3 = /* @__PURE__ */ new Set();
      return (s3) => {
        const i3 = e3(s3);
        return !t3.has(i3) && (t3.add(i3), true);
      };
    }, t2.firstOrDefault = function(e3, t3) {
      return e3.length > 0 ? e3[0] : t3;
    }, t2.lastOrDefault = function(e3, t3) {
      return e3.length > 0 ? e3[e3.length - 1] : t3;
    }, t2.commonPrefixLength = function(e3, t3, s3 = (e4, t4) => e4 === t4) {
      let i3 = 0;
      for (let r3 = 0, n3 = Math.min(e3.length, t3.length); r3 < n3 && s3(e3[r3], t3[r3]); r3++) i3++;
      return i3;
    }, t2.range = function(e3, t3) {
      let s3 = "number" == typeof t3 ? e3 : 0;
      "number" == typeof t3 ? s3 = e3 : (s3 = 0, t3 = e3);
      const i3 = [];
      if (s3 <= t3) for (let e4 = s3; e4 < t3; e4++) i3.push(e4);
      else for (let e4 = s3; e4 > t3; e4--) i3.push(e4);
      return i3;
    }, t2.index = function(e3, t3, s3) {
      return e3.reduce(((e4, i3) => (e4[t3(i3)] = s3 ? s3(i3) : i3, e4)), /* @__PURE__ */ Object.create(null));
    }, t2.insert = function(e3, t3) {
      return e3.push(t3), () => h(e3, t3);
    }, t2.remove = h, t2.arrayInsert = function(e3, t3, s3) {
      const i3 = e3.slice(0, t3), r3 = e3.slice(t3);
      return i3.concat(s3, r3);
    }, t2.shuffle = function(e3, t3) {
      let s3;
      if ("number" == typeof t3) {
        let e4 = t3;
        s3 = () => {
          const t4 = 179426549 * Math.sin(e4++);
          return t4 - Math.floor(t4);
        };
      } else s3 = Math.random;
      for (let t4 = e3.length - 1; t4 > 0; t4 -= 1) {
        const i3 = Math.floor(s3() * (t4 + 1)), r3 = e3[t4];
        e3[t4] = e3[i3], e3[i3] = r3;
      }
    }, t2.pushToStart = function(e3, t3) {
      const s3 = e3.indexOf(t3);
      s3 > -1 && (e3.splice(s3, 1), e3.unshift(t3));
    }, t2.pushToEnd = function(e3, t3) {
      const s3 = e3.indexOf(t3);
      s3 > -1 && (e3.splice(s3, 1), e3.push(t3));
    }, t2.pushMany = function(e3, t3) {
      for (const s3 of t3) e3.push(s3);
    }, t2.mapArrayOrNot = function(e3, t3) {
      return Array.isArray(e3) ? e3.map(t3) : t3(e3);
    }, t2.asArray = function(e3) {
      return Array.isArray(e3) ? e3 : [e3];
    }, t2.getRandomElement = function(e3) {
      return e3[Math.floor(Math.random() * e3.length)];
    }, t2.insertInto = c, t2.splice = function(e3, t3, s3, i3) {
      const r3 = l(e3, t3);
      let n3 = e3.splice(r3, s3);
      return void 0 === n3 && (n3 = []), c(e3, r3, i3), n3;
    }, t2.compareBy = function(e3, t3) {
      return (s3, i3) => t3(e3(s3), e3(i3));
    }, t2.tieBreakComparators = function(...e3) {
      return (t3, s3) => {
        for (const i3 of e3) {
          const e4 = i3(t3, s3);
          if (!u.isNeitherLessOrGreaterThan(e4)) return e4;
        }
        return u.neitherLessOrGreaterThan;
      };
    }, t2.reverseOrder = function(e3) {
      return (t3, s3) => -e3(t3, s3);
    };
    const i2 = s2(9807), r2 = s2(8297);
    function n2(e3, t3) {
      let s3 = 0, i3 = e3 - 1;
      for (; s3 <= i3; ) {
        const e4 = (s3 + i3) / 2 | 0, r3 = t3(e4);
        if (r3 < 0) s3 = e4 + 1;
        else {
          if (!(r3 > 0)) return e4;
          i3 = e4 - 1;
        }
      }
      return -(s3 + 1);
    }
    function o(e3, t3, s3) {
      const i3 = [];
      function r3(e4, t4, s4) {
        if (0 === t4 && 0 === s4.length) return;
        const r4 = i3[i3.length - 1];
        r4 && r4.start + r4.deleteCount === e4 ? (r4.deleteCount += t4, r4.toInsert.push(...s4)) : i3.push({ start: e4, deleteCount: t4, toInsert: s4 });
      }
      let n3 = 0, o2 = 0;
      for (; ; ) {
        if (n3 === e3.length) {
          r3(n3, 0, t3.slice(o2));
          break;
        }
        if (o2 === t3.length) {
          r3(n3, e3.length - n3, []);
          break;
        }
        const i4 = e3[n3], a2 = t3[o2], h2 = s3(i4, a2);
        0 === h2 ? (n3 += 1, o2 += 1) : h2 < 0 ? (r3(n3, 1, []), n3 += 1) : h2 > 0 && (r3(n3, 0, [a2]), o2 += 1);
      }
      return i3;
    }
    function a(e3, t3, s3, i3, n3) {
      for (const o2 = s3.length; i3 < n3; i3++) {
        const n4 = e3[i3];
        if (t3(n4, s3[o2 - 1]) < 0) {
          s3.pop();
          const e4 = (0, r2.findFirstIdxMonotonousOrArrLen)(s3, ((e5) => t3(n4, e5) < 0));
          s3.splice(e4, 0, n4);
        }
      }
    }
    function h(e3, t3) {
      const s3 = e3.indexOf(t3);
      if (s3 > -1) return e3.splice(s3, 1), t3;
    }
    function c(e3, t3, s3) {
      const i3 = l(e3, t3), r3 = e3.length, n3 = s3.length;
      e3.length = r3 + n3;
      for (let t4 = r3 - 1; t4 >= i3; t4--) e3[t4 + n3] = e3[t4];
      for (let t4 = 0; t4 < n3; t4++) e3[t4 + i3] = s3[t4];
    }
    function l(e3, t3) {
      return t3 < 0 ? Math.max(t3 + e3.length, 0) : Math.min(t3, e3.length);
    }
    var u;
    !(function(e3) {
      e3.isLessThan = function(e4) {
        return e4 < 0;
      }, e3.isLessThanOrEqual = function(e4) {
        return e4 <= 0;
      }, e3.isGreaterThan = function(e4) {
        return e4 > 0;
      }, e3.isNeitherLessOrGreaterThan = function(e4) {
        return 0 === e4;
      }, e3.greaterThan = 1, e3.lessThan = -1, e3.neitherLessOrGreaterThan = 0;
    })(u || (t2.CompareResult = u = {})), t2.numberComparator = (e3, t3) => e3 - t3, t2.booleanComparator = (e3, s3) => (0, t2.numberComparator)(e3 ? 1 : 0, s3 ? 1 : 0), t2.ArrayQueue = class {
      constructor(e3) {
        this.items = e3, this.firstIdx = 0, this.lastIdx = this.items.length - 1;
      }
      get length() {
        return this.lastIdx - this.firstIdx + 1;
      }
      takeWhile(e3) {
        let t3 = this.firstIdx;
        for (; t3 < this.items.length && e3(this.items[t3]); ) t3++;
        const s3 = t3 === this.firstIdx ? null : this.items.slice(this.firstIdx, t3);
        return this.firstIdx = t3, s3;
      }
      takeFromEndWhile(e3) {
        let t3 = this.lastIdx;
        for (; t3 >= 0 && e3(this.items[t3]); ) t3--;
        const s3 = t3 === this.lastIdx ? null : this.items.slice(t3 + 1, this.lastIdx + 1);
        return this.lastIdx = t3, s3;
      }
      peek() {
        if (0 !== this.length) return this.items[this.firstIdx];
      }
      peekLast() {
        if (0 !== this.length) return this.items[this.lastIdx];
      }
      dequeue() {
        const e3 = this.items[this.firstIdx];
        return this.firstIdx++, e3;
      }
      removeLast() {
        const e3 = this.items[this.lastIdx];
        return this.lastIdx--, e3;
      }
      takeCount(e3) {
        const t3 = this.items.slice(this.firstIdx, this.firstIdx + e3);
        return this.firstIdx += e3, t3;
      }
    };
    const _d = class _d {
      constructor(e3) {
        this.iterate = e3;
      }
      forEach(e3) {
        this.iterate(((t3) => (e3(t3), true)));
      }
      toArray() {
        const e3 = [];
        return this.iterate(((t3) => (e3.push(t3), true))), e3;
      }
      filter(e3) {
        return new _d(((t3) => this.iterate(((s3) => !e3(s3) || t3(s3)))));
      }
      map(e3) {
        return new _d(((t3) => this.iterate(((s3) => t3(e3(s3))))));
      }
      some(e3) {
        let t3 = false;
        return this.iterate(((s3) => (t3 = e3(s3), !t3))), t3;
      }
      findFirst(e3) {
        let t3;
        return this.iterate(((s3) => !e3(s3) || (t3 = s3, false))), t3;
      }
      findLast(e3) {
        let t3;
        return this.iterate(((s3) => (e3(s3) && (t3 = s3), true))), t3;
      }
      findLastMaxBy(e3) {
        let t3, s3 = true;
        return this.iterate(((i3) => ((s3 || u.isGreaterThan(e3(i3, t3))) && (s3 = false, t3 = i3), true))), t3;
      }
    };
    _d.empty = new _d(((e3) => {
    }));
    let d = _d;
    t2.CallbackIterable = d;
    class f {
      constructor(e3) {
        this._indexMap = e3;
      }
      static createSortPermutation(e3, t3) {
        const s3 = Array.from(e3.keys()).sort(((s4, i3) => t3(e3[s4], e3[i3])));
        return new f(s3);
      }
      apply(e3) {
        return e3.map(((t3, s3) => e3[this._indexMap[s3]]));
      }
      inverse() {
        const e3 = this._indexMap.slice();
        for (let t3 = 0; t3 < this._indexMap.length; t3++) e3[this._indexMap[t3]] = t3;
        return new f(e3);
      }
    }
    t2.Permutation = f;
  }, 8297: (e2, t2) => {
    function s2(e3, t3, s3 = e3.length - 1) {
      for (let i3 = s3; i3 >= 0; i3--) if (t3(e3[i3])) return i3;
      return -1;
    }
    function i2(e3, t3, s3 = 0, i3 = e3.length) {
      let r3 = s3, n3 = i3;
      for (; r3 < n3; ) {
        const s4 = Math.floor((r3 + n3) / 2);
        t3(e3[s4]) ? r3 = s4 + 1 : n3 = s4;
      }
      return r3 - 1;
    }
    function r2(e3, t3, s3 = 0, i3 = e3.length) {
      let r3 = s3, n3 = i3;
      for (; r3 < n3; ) {
        const s4 = Math.floor((r3 + n3) / 2);
        t3(e3[s4]) ? n3 = s4 : r3 = s4 + 1;
      }
      return r3;
    }
    Object.defineProperty(t2, "__esModule", { value: true }), t2.MonotonousArray = void 0, t2.findLast = function(e3, t3) {
      const i3 = s2(e3, t3);
      if (-1 !== i3) return e3[i3];
    }, t2.findLastIdx = s2, t2.findLastMonotonous = function(e3, t3) {
      const s3 = i2(e3, t3);
      return -1 === s3 ? void 0 : e3[s3];
    }, t2.findLastIdxMonotonous = i2, t2.findFirstMonotonous = function(e3, t3) {
      const s3 = r2(e3, t3);
      return s3 === e3.length ? void 0 : e3[s3];
    }, t2.findFirstIdxMonotonousOrArrLen = r2, t2.findFirstIdxMonotonous = function(e3, t3, s3 = 0, i3 = e3.length) {
      const n3 = r2(e3, t3, s3, i3);
      return n3 === e3.length ? -1 : n3;
    }, t2.findFirstMax = o, t2.findLastMax = function(e3, t3) {
      if (0 === e3.length) return;
      let s3 = e3[0];
      for (let i3 = 1; i3 < e3.length; i3++) {
        const r3 = e3[i3];
        t3(r3, s3) >= 0 && (s3 = r3);
      }
      return s3;
    }, t2.findFirstMin = function(e3, t3) {
      return o(e3, ((e4, s3) => -t3(e4, s3)));
    }, t2.findMaxIdx = function(e3, t3) {
      if (0 === e3.length) return -1;
      let s3 = 0;
      for (let i3 = 1; i3 < e3.length; i3++) t3(e3[i3], e3[s3]) > 0 && (s3 = i3);
      return s3;
    }, t2.mapFindFirst = function(e3, t3) {
      for (const s3 of e3) {
        const e4 = t3(s3);
        if (void 0 !== e4) return e4;
      }
    };
    const _n = class _n {
      constructor(e3) {
        this._array = e3, this._findLastMonotonousLastIdx = 0;
      }
      findLastMonotonous(e3) {
        if (_n.assertInvariants) {
          if (this._prevFindLastPredicate) {
            for (const t4 of this._array) if (this._prevFindLastPredicate(t4) && !e3(t4)) throw new Error("MonotonousArray: current predicate must be weaker than (or equal to) the previous predicate.");
          }
          this._prevFindLastPredicate = e3;
        }
        const t3 = i2(this._array, e3, this._findLastMonotonousLastIdx);
        return this._findLastMonotonousLastIdx = t3 + 1, -1 === t3 ? void 0 : this._array[t3];
      }
    };
    _n.assertInvariants = false;
    let n2 = _n;
    function o(e3, t3) {
      if (0 === e3.length) return;
      let s3 = e3[0];
      for (let i3 = 1; i3 < e3.length; i3++) {
        const r3 = e3[i3];
        t3(r3, s3) > 0 && (s3 = r3);
      }
      return s3;
    }
    t2.MonotonousArray = n2;
  }, 9087: (e2, t2) => {
    var s2;
    Object.defineProperty(t2, "__esModule", { value: true }), t2.SetWithKey = void 0, t2.groupBy = function(e3, t3) {
      const s3 = /* @__PURE__ */ Object.create(null);
      for (const i3 of e3) {
        const e4 = t3(i3);
        let r2 = s3[e4];
        r2 || (r2 = s3[e4] = []), r2.push(i3);
      }
      return s3;
    }, t2.diffSets = function(e3, t3) {
      const s3 = [], i3 = [];
      for (const i4 of e3) t3.has(i4) || s3.push(i4);
      for (const s4 of t3) e3.has(s4) || i3.push(s4);
      return { removed: s3, added: i3 };
    }, t2.diffMaps = function(e3, t3) {
      const s3 = [], i3 = [];
      for (const [i4, r2] of e3) t3.has(i4) || s3.push(r2);
      for (const [s4, r2] of t3) e3.has(s4) || i3.push(r2);
      return { removed: s3, added: i3 };
    }, t2.intersection = function(e3, t3) {
      const s3 = /* @__PURE__ */ new Set();
      for (const i3 of t3) e3.has(i3) && s3.add(i3);
      return s3;
    };
    class i2 {
      constructor(e3, t3) {
        this.toKey = t3, this._map = /* @__PURE__ */ new Map(), this[s2] = "SetWithKey";
        for (const t4 of e3) this.add(t4);
      }
      get size() {
        return this._map.size;
      }
      add(e3) {
        const t3 = this.toKey(e3);
        return this._map.set(t3, e3), this;
      }
      delete(e3) {
        return this._map.delete(this.toKey(e3));
      }
      has(e3) {
        return this._map.has(this.toKey(e3));
      }
      *entries() {
        for (const e3 of this._map.values()) yield [e3, e3];
      }
      keys() {
        return this.values();
      }
      *values() {
        for (const e3 of this._map.values()) yield e3;
      }
      clear() {
        this._map.clear();
      }
      forEach(e3, t3) {
        this._map.forEach(((s3) => e3.call(t3, s3, s3, this)));
      }
      [Symbol.iterator]() {
        return this.values();
      }
    }
    s2 = Symbol.toStringTag;
    t2.SetWithKey = i2;
  }, 9807: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.BugIndicatingError = t2.ErrorNoTelemetry = t2.ExpectedError = t2.NotSupportedError = t2.NotImplementedError = t2.ReadonlyError = t2.CancellationError = t2.errorHandler = t2.ErrorHandler = void 0, t2.setUnexpectedErrorHandler = function(e3) {
      t2.errorHandler.setUnexpectedErrorHandler(e3);
    }, t2.isSigPipeError = function(e3) {
      var _a;
      if (!e3 || "object" != typeof e3) return false;
      const t3 = e3;
      return "EPIPE" === t3.code && "WRITE" === ((_a = t3.syscall) == null ? void 0 : _a.toUpperCase());
    }, t2.onUnexpectedError = function(e3) {
      r2(e3) || t2.errorHandler.onUnexpectedError(e3);
    }, t2.onUnexpectedExternalError = function(e3) {
      r2(e3) || t2.errorHandler.onUnexpectedExternalError(e3);
    }, t2.transformErrorForSerialization = function(e3) {
      if (e3 instanceof Error) {
        const { name: t3, message: s3 } = e3;
        return { $isError: true, name: t3, message: s3, stack: e3.stacktrace || e3.stack, noTelemetry: l.isErrorNoTelemetry(e3) };
      }
      return e3;
    }, t2.transformErrorFromSerialization = function(e3) {
      let t3;
      return e3.noTelemetry ? t3 = new l() : (t3 = new Error(), t3.name = e3.name), t3.message = e3.message, t3.stack = e3.stack, t3;
    }, t2.isCancellationError = r2, t2.canceled = function() {
      const e3 = new Error(i2);
      return e3.name = e3.message, e3;
    }, t2.illegalArgument = function(e3) {
      return e3 ? new Error(`Illegal argument: ${e3}`) : new Error("Illegal argument");
    }, t2.illegalState = function(e3) {
      return e3 ? new Error(`Illegal state: ${e3}`) : new Error("Illegal state");
    }, t2.getErrorMessage = function(e3) {
      return e3 ? e3.message ? e3.message : e3.stack ? e3.stack.split("\n")[0] : String(e3) : "Error";
    };
    class s2 {
      constructor() {
        this.listeners = [], this.unexpectedErrorHandler = function(e3) {
          setTimeout((() => {
            if (e3.stack) {
              if (l.isErrorNoTelemetry(e3)) throw new l(e3.message + "\n\n" + e3.stack);
              throw new Error(e3.message + "\n\n" + e3.stack);
            }
            throw e3;
          }), 0);
        };
      }
      addListener(e3) {
        return this.listeners.push(e3), () => {
          this._removeListener(e3);
        };
      }
      emit(e3) {
        this.listeners.forEach(((t3) => {
          t3(e3);
        }));
      }
      _removeListener(e3) {
        this.listeners.splice(this.listeners.indexOf(e3), 1);
      }
      setUnexpectedErrorHandler(e3) {
        this.unexpectedErrorHandler = e3;
      }
      getUnexpectedErrorHandler() {
        return this.unexpectedErrorHandler;
      }
      onUnexpectedError(e3) {
        this.unexpectedErrorHandler(e3), this.emit(e3);
      }
      onUnexpectedExternalError(e3) {
        this.unexpectedErrorHandler(e3);
      }
    }
    t2.ErrorHandler = s2, t2.errorHandler = new s2();
    const i2 = "Canceled";
    function r2(e3) {
      return e3 instanceof n2 || e3 instanceof Error && e3.name === i2 && e3.message === i2;
    }
    class n2 extends Error {
      constructor() {
        super(i2), this.name = this.message;
      }
    }
    t2.CancellationError = n2;
    class o extends TypeError {
      constructor(e3) {
        super(e3 ? `${e3} is read-only and cannot be changed` : "Cannot change read-only property");
      }
    }
    t2.ReadonlyError = o;
    class a extends Error {
      constructor(e3) {
        super("NotImplemented"), e3 && (this.message = e3);
      }
    }
    t2.NotImplementedError = a;
    class h extends Error {
      constructor(e3) {
        super("NotSupported"), e3 && (this.message = e3);
      }
    }
    t2.NotSupportedError = h;
    class c extends Error {
      constructor() {
        super(...arguments), this.isExpected = true;
      }
    }
    t2.ExpectedError = c;
    class l extends Error {
      constructor(e3) {
        super(e3), this.name = "CodeExpectedError";
      }
      static fromError(e3) {
        if (e3 instanceof l) return e3;
        const t3 = new l();
        return t3.message = e3.message, t3.stack = e3.stack, t3;
      }
      static isErrorNoTelemetry(e3) {
        return "CodeExpectedError" === e3.name;
      }
    }
    t2.ErrorNoTelemetry = l;
    class u extends Error {
      constructor(e3) {
        super(e3 || "An unexpected bug occurred."), Object.setPrototypeOf(this, u.prototype);
      }
    }
    t2.BugIndicatingError = u;
  }, 802: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.ValueWithChangeEvent = t2.Relay = t2.EventBufferer = t2.DynamicListEventMultiplexer = t2.EventMultiplexer = t2.MicrotaskEmitter = t2.DebounceEmitter = t2.PauseableEmitter = t2.AsyncEmitter = t2.createEventDeliveryQueue = t2.Emitter = t2.ListenerRefusalError = t2.ListenerLeakError = t2.EventProfiling = t2.Event = void 0, t2.setGlobalLeakWarningThreshold = function(e3) {
      const t3 = l;
      return l = e3, { dispose() {
        l = t3;
      } };
    };
    const i2 = s2(9807), r2 = s2(8841), n2 = s2(7150), o = s2(6317), a = s2(9725);
    var h;
    !(function(e3) {
      function t3(e4) {
        return (t4, s4 = null, i4) => {
          let r4, n3 = false;
          return r4 = e4(((e5) => {
            if (!n3) return r4 ? r4.dispose() : n3 = true, t4.call(s4, e5);
          }), null, i4), n3 && r4.dispose(), r4;
        };
      }
      function s3(e4, t4, s4) {
        return r3(((s5, i4 = null, r4) => e4(((e5) => s5.call(i4, t4(e5))), null, r4)), s4);
      }
      function i3(e4, t4, s4) {
        return r3(((s5, i4 = null, r4) => e4(((e5) => t4(e5) && s5.call(i4, e5)), null, r4)), s4);
      }
      function r3(e4, t4) {
        let s4;
        const i4 = new v({ onWillAddFirstListener() {
          s4 = e4(i4.fire, i4);
        }, onDidRemoveLastListener() {
          s4 == null ? void 0 : s4.dispose();
        } });
        return t4 == null ? void 0 : t4.add(i4), i4.event;
      }
      function o2(e4, t4, s4 = 100, i4 = false, r4 = false, n3, o3) {
        let a3, h3, c3, l2, u2 = 0;
        const d2 = new v({ leakWarningThreshold: n3, onWillAddFirstListener() {
          a3 = e4(((e5) => {
            u2++, h3 = t4(h3, e5), i4 && !c3 && (d2.fire(h3), h3 = void 0), l2 = () => {
              const e6 = h3;
              h3 = void 0, c3 = void 0, (!i4 || u2 > 1) && d2.fire(e6), u2 = 0;
            }, "number" == typeof s4 ? (clearTimeout(c3), c3 = setTimeout(l2, s4)) : void 0 === c3 && (c3 = 0, queueMicrotask(l2));
          }));
        }, onWillRemoveListener() {
          r4 && u2 > 0 && (l2 == null ? void 0 : l2());
        }, onDidRemoveLastListener() {
          l2 = void 0, a3.dispose();
        } });
        return o3 == null ? void 0 : o3.add(d2), d2.event;
      }
      e3.None = () => n2.Disposable.None, e3.defer = function(e4, t4) {
        return o2(e4, (() => {
        }), 0, void 0, true, void 0, t4);
      }, e3.once = t3, e3.map = s3, e3.forEach = function(e4, t4, s4) {
        return r3(((s5, i4 = null, r4) => e4(((e5) => {
          t4(e5), s5.call(i4, e5);
        }), null, r4)), s4);
      }, e3.filter = i3, e3.signal = function(e4) {
        return e4;
      }, e3.any = function(...e4) {
        return (t4, s4 = null, i4) => {
          return r4 = (0, n2.combinedDisposable)(...e4.map(((e5) => e5(((e6) => t4.call(s4, e6)))))), (o3 = i4) instanceof Array ? o3.push(r4) : o3 && o3.add(r4), r4;
          var r4, o3;
        };
      }, e3.reduce = function(e4, t4, i4, r4) {
        let n3 = i4;
        return s3(e4, ((e5) => (n3 = t4(n3, e5), n3)), r4);
      }, e3.debounce = o2, e3.accumulate = function(t4, s4 = 0, i4) {
        return e3.debounce(t4, ((e4, t5) => e4 ? (e4.push(t5), e4) : [t5]), s4, void 0, true, void 0, i4);
      }, e3.latch = function(e4, t4 = (e5, t5) => e5 === t5, s4) {
        let r4, n3 = true;
        return i3(e4, ((e5) => {
          const s5 = n3 || !t4(e5, r4);
          return n3 = false, r4 = e5, s5;
        }), s4);
      }, e3.split = function(t4, s4, i4) {
        return [e3.filter(t4, s4, i4), e3.filter(t4, ((e4) => !s4(e4)), i4)];
      }, e3.buffer = function(e4, t4 = false, s4 = [], i4) {
        let r4 = s4.slice(), n3 = e4(((e5) => {
          r4 ? r4.push(e5) : a3.fire(e5);
        }));
        i4 && i4.add(n3);
        const o3 = () => {
          r4 == null ? void 0 : r4.forEach(((e5) => a3.fire(e5))), r4 = null;
        }, a3 = new v({ onWillAddFirstListener() {
          n3 || (n3 = e4(((e5) => a3.fire(e5))), i4 && i4.add(n3));
        }, onDidAddFirstListener() {
          r4 && (t4 ? setTimeout(o3) : o3());
        }, onDidRemoveLastListener() {
          n3 && n3.dispose(), n3 = null;
        } });
        return i4 && i4.add(a3), a3.event;
      }, e3.chain = function(e4, t4) {
        return (s4, i4, r4) => {
          const n3 = t4(new h2());
          return e4((function(e5) {
            const t5 = n3.evaluate(e5);
            t5 !== a2 && s4.call(i4, t5);
          }), void 0, r4);
        };
      };
      const a2 = /* @__PURE__ */ Symbol("HaltChainable");
      class h2 {
        constructor() {
          this.steps = [];
        }
        map(e4) {
          return this.steps.push(e4), this;
        }
        forEach(e4) {
          return this.steps.push(((t4) => (e4(t4), t4))), this;
        }
        filter(e4) {
          return this.steps.push(((t4) => e4(t4) ? t4 : a2)), this;
        }
        reduce(e4, t4) {
          let s4 = t4;
          return this.steps.push(((t5) => (s4 = e4(s4, t5), s4))), this;
        }
        latch(e4 = (e5, t4) => e5 === t4) {
          let t4, s4 = true;
          return this.steps.push(((i4) => {
            const r4 = s4 || !e4(i4, t4);
            return s4 = false, t4 = i4, r4 ? i4 : a2;
          })), this;
        }
        evaluate(e4) {
          for (const t4 of this.steps) if ((e4 = t4(e4)) === a2) break;
          return e4;
        }
      }
      e3.fromNodeEventEmitter = function(e4, t4, s4 = (e5) => e5) {
        const i4 = (...e5) => r4.fire(s4(...e5)), r4 = new v({ onWillAddFirstListener: () => e4.on(t4, i4), onDidRemoveLastListener: () => e4.removeListener(t4, i4) });
        return r4.event;
      }, e3.fromDOMEventEmitter = function(e4, t4, s4 = (e5) => e5) {
        const i4 = (...e5) => r4.fire(s4(...e5)), r4 = new v({ onWillAddFirstListener: () => e4.addEventListener(t4, i4), onDidRemoveLastListener: () => e4.removeEventListener(t4, i4) });
        return r4.event;
      }, e3.toPromise = function(e4) {
        return new Promise(((s4) => t3(e4)(s4)));
      }, e3.fromPromise = function(e4) {
        const t4 = new v();
        return e4.then(((e5) => {
          t4.fire(e5);
        }), (() => {
          t4.fire(void 0);
        })).finally((() => {
          t4.dispose();
        })), t4.event;
      }, e3.forward = function(e4, t4) {
        return e4(((e5) => t4.fire(e5)));
      }, e3.runAndSubscribe = function(e4, t4, s4) {
        return t4(s4), e4(((e5) => t4(e5)));
      };
      class c2 {
        constructor(e4, t4) {
          this._observable = e4, this._counter = 0, this._hasChanged = false;
          const s4 = { onWillAddFirstListener: () => {
            e4.addObserver(this);
          }, onDidRemoveLastListener: () => {
            e4.removeObserver(this);
          } };
          this.emitter = new v(s4), t4 && t4.add(this.emitter);
        }
        beginUpdate(e4) {
          this._counter++;
        }
        handlePossibleChange(e4) {
        }
        handleChange(e4, t4) {
          this._hasChanged = true;
        }
        endUpdate(e4) {
          this._counter--, 0 === this._counter && (this._observable.reportChanges(), this._hasChanged && (this._hasChanged = false, this.emitter.fire(this._observable.get())));
        }
      }
      e3.fromObservable = function(e4, t4) {
        return new c2(e4, t4).emitter.event;
      }, e3.fromObservableLight = function(e4) {
        return (t4, s4, i4) => {
          let r4 = 0, o3 = false;
          const a3 = { beginUpdate() {
            r4++;
          }, endUpdate() {
            r4--, 0 === r4 && (e4.reportChanges(), o3 && (o3 = false, t4.call(s4)));
          }, handlePossibleChange() {
          }, handleChange() {
            o3 = true;
          } };
          e4.addObserver(a3), e4.reportChanges();
          const h3 = { dispose() {
            e4.removeObserver(a3);
          } };
          return i4 instanceof n2.DisposableStore ? i4.add(h3) : Array.isArray(i4) && i4.push(h3), h3;
        };
      };
    })(h || (t2.Event = h = {}));
    const _c = class _c {
      constructor(e3) {
        this.listenerCount = 0, this.invocationCount = 0, this.elapsedOverall = 0, this.durations = [], this.name = `${e3}_${_c._idPool++}`, _c.all.add(this);
      }
      start(e3) {
        this._stopWatch = new a.StopWatch(), this.listenerCount = e3;
      }
      stop() {
        if (this._stopWatch) {
          const e3 = this._stopWatch.elapsed();
          this.durations.push(e3), this.elapsedOverall += e3, this.invocationCount += 1, this._stopWatch = void 0;
        }
      }
    };
    _c.all = /* @__PURE__ */ new Set();
    _c._idPool = 0;
    let c = _c;
    t2.EventProfiling = c;
    let l = -1;
    const _u = class _u {
      constructor(e3, t3, s3 = (_u._idPool++).toString(16).padStart(3, "0")) {
        this._errorHandler = e3, this.threshold = t3, this.name = s3, this._warnCountdown = 0;
      }
      dispose() {
        var _a;
        (_a = this._stacks) == null ? void 0 : _a.clear();
      }
      check(e3, t3) {
        const s3 = this.threshold;
        if (s3 <= 0 || t3 < s3) return;
        this._stacks || (this._stacks = /* @__PURE__ */ new Map());
        const i3 = this._stacks.get(e3.value) || 0;
        if (this._stacks.set(e3.value, i3 + 1), this._warnCountdown -= 1, this._warnCountdown <= 0) {
          this._warnCountdown = 0.5 * s3;
          const [e4, i4] = this.getMostFrequentStack(), r3 = `[${this.name}] potential listener LEAK detected, having ${t3} listeners already. MOST frequent listener (${i4}):`;
          console.warn(r3), console.warn(e4);
          const n3 = new f(r3, e4);
          this._errorHandler(n3);
        }
        return () => {
          const t4 = this._stacks.get(e3.value) || 0;
          this._stacks.set(e3.value, t4 - 1);
        };
      }
      getMostFrequentStack() {
        if (!this._stacks) return;
        let e3, t3 = 0;
        for (const [s3, i3] of this._stacks) (!e3 || t3 < i3) && (e3 = [s3, i3], t3 = i3);
        return e3;
      }
    };
    _u._idPool = 1;
    let u = _u;
    class d {
      static create() {
        var _a;
        const e3 = new Error();
        return new d((_a = e3.stack) != null ? _a : "");
      }
      constructor(e3) {
        this.value = e3;
      }
      print() {
        console.warn(this.value.split("\n").slice(2).join("\n"));
      }
    }
    class f extends Error {
      constructor(e3, t3) {
        super(e3), this.name = "ListenerLeakError", this.stack = t3;
      }
    }
    t2.ListenerLeakError = f;
    class _ extends Error {
      constructor(e3, t3) {
        super(e3), this.name = "ListenerRefusalError", this.stack = t3;
      }
    }
    t2.ListenerRefusalError = _;
    let p = 0;
    class g {
      constructor(e3) {
        this.value = e3, this.id = p++;
      }
    }
    class v {
      constructor(e3) {
        var _a, _b, _c2, _d, _e, _f;
        this._size = 0, this._options = e3, this._leakageMon = l > 0 || ((_a = this._options) == null ? void 0 : _a.leakWarningThreshold) ? new u((_b = e3 == null ? void 0 : e3.onListenerError) != null ? _b : i2.onUnexpectedError, (_d = (_c2 = this._options) == null ? void 0 : _c2.leakWarningThreshold) != null ? _d : l) : void 0, this._perfMon = ((_e = this._options) == null ? void 0 : _e._profName) ? new c(this._options._profName) : void 0, this._deliveryQueue = (_f = this._options) == null ? void 0 : _f.deliveryQueue;
      }
      dispose() {
        var _a, _b, _c2, _d;
        this._disposed || (this._disposed = true, ((_a = this._deliveryQueue) == null ? void 0 : _a.current) === this && this._deliveryQueue.reset(), this._listeners && (this._listeners = void 0, this._size = 0), (_c2 = (_b = this._options) == null ? void 0 : _b.onDidRemoveLastListener) == null ? void 0 : _c2.call(_b), (_d = this._leakageMon) == null ? void 0 : _d.dispose());
      }
      get event() {
        var _a;
        return (_a = this._event) != null ? _a : this._event = (e3, t3, s3) => {
          var _a2, _b, _c2, _d, _e, _f, _g;
          if (this._leakageMon && this._size > __pow(this._leakageMon.threshold, 2)) {
            const e4 = `[${this._leakageMon.name}] REFUSES to accept new listeners because it exceeded its threshold by far (${this._size} vs ${this._leakageMon.threshold})`;
            console.warn(e4);
            const t4 = (_a2 = this._leakageMon.getMostFrequentStack()) != null ? _a2 : ["UNKNOWN stack", -1], s4 = new _(`${e4}. HINT: Stack shows most frequent listener (${t4[1]}-times)`, t4[0]);
            return (((_b = this._options) == null ? void 0 : _b.onListenerError) || i2.onUnexpectedError)(s4), n2.Disposable.None;
          }
          if (this._disposed) return n2.Disposable.None;
          t3 && (e3 = e3.bind(t3));
          const r3 = new g(e3);
          let o2;
          this._leakageMon && this._size >= Math.ceil(0.2 * this._leakageMon.threshold) && (r3.stack = d.create(), o2 = this._leakageMon.check(r3.stack, this._size + 1)), this._listeners ? this._listeners instanceof g ? ((_c2 = this._deliveryQueue) != null ? _c2 : this._deliveryQueue = new m(), this._listeners = [this._listeners, r3]) : this._listeners.push(r3) : ((_e = (_d = this._options) == null ? void 0 : _d.onWillAddFirstListener) == null ? void 0 : _e.call(_d, this), this._listeners = r3, (_g = (_f = this._options) == null ? void 0 : _f.onDidAddFirstListener) == null ? void 0 : _g.call(_f, this)), this._size++;
          const a2 = (0, n2.toDisposable)((() => {
            o2 == null ? void 0 : o2(), this._removeListener(r3);
          }));
          return s3 instanceof n2.DisposableStore ? s3.add(a2) : Array.isArray(s3) && s3.push(a2), a2;
        }, this._event;
      }
      _removeListener(e3) {
        var _a, _b, _c2, _d;
        if ((_b = (_a = this._options) == null ? void 0 : _a.onWillRemoveListener) == null ? void 0 : _b.call(_a, this), !this._listeners) return;
        if (1 === this._size) return this._listeners = void 0, (_d = (_c2 = this._options) == null ? void 0 : _c2.onDidRemoveLastListener) == null ? void 0 : _d.call(_c2, this), void (this._size = 0);
        const t3 = this._listeners, s3 = t3.indexOf(e3);
        if (-1 === s3) throw console.log("disposed?", this._disposed), console.log("size?", this._size), console.log("arr?", JSON.stringify(this._listeners)), new Error("Attempted to dispose unknown listener");
        this._size--, t3[s3] = void 0;
        const i3 = this._deliveryQueue.current === this;
        if (2 * this._size <= t3.length) {
          let e4 = 0;
          for (let s4 = 0; s4 < t3.length; s4++) t3[s4] ? t3[e4++] = t3[s4] : i3 && (this._deliveryQueue.end--, e4 < this._deliveryQueue.i && this._deliveryQueue.i--);
          t3.length = e4;
        }
      }
      _deliver(e3, t3) {
        var _a;
        if (!e3) return;
        const s3 = ((_a = this._options) == null ? void 0 : _a.onListenerError) || i2.onUnexpectedError;
        if (s3) try {
          e3.value(t3);
        } catch (e4) {
          s3(e4);
        }
        else e3.value(t3);
      }
      _deliverQueue(e3) {
        const t3 = e3.current._listeners;
        for (; e3.i < e3.end; ) this._deliver(t3[e3.i++], e3.value);
        e3.reset();
      }
      fire(e3) {
        var _a, _b, _c2, _d;
        if (((_a = this._deliveryQueue) == null ? void 0 : _a.current) && (this._deliverQueue(this._deliveryQueue), (_b = this._perfMon) == null ? void 0 : _b.stop()), (_c2 = this._perfMon) == null ? void 0 : _c2.start(this._size), this._listeners) if (this._listeners instanceof g) this._deliver(this._listeners, e3);
        else {
          const t3 = this._deliveryQueue;
          t3.enqueue(this, e3, this._listeners.length), this._deliverQueue(t3);
        }
        (_d = this._perfMon) == null ? void 0 : _d.stop();
      }
      hasListeners() {
        return this._size > 0;
      }
    }
    t2.Emitter = v, t2.createEventDeliveryQueue = () => new m();
    class m {
      constructor() {
        this.i = -1, this.end = 0;
      }
      enqueue(e3, t3, s3) {
        this.i = 0, this.end = s3, this.current = e3, this.value = t3;
      }
      reset() {
        this.i = this.end, this.current = void 0, this.value = void 0;
      }
    }
    t2.AsyncEmitter = class extends v {
      fireAsync(e3, t3, s3) {
        return __async(this, null, function* () {
          if (this._listeners) for (this._asyncDeliveryQueue || (this._asyncDeliveryQueue = new o.LinkedList()), ((e4, t4) => {
            if (e4 instanceof g) t4(e4);
            else for (let s4 = 0; s4 < e4.length; s4++) {
              const i3 = e4[s4];
              i3 && t4(i3);
            }
          })(this._listeners, ((t4) => this._asyncDeliveryQueue.push([t4.value, e3]))); this._asyncDeliveryQueue.size > 0 && !t3.isCancellationRequested; ) {
            const [e4, r3] = this._asyncDeliveryQueue.shift(), n3 = [], o2 = __spreadProps(__spreadValues({}, r3), { token: t3, waitUntil: (t4) => {
              if (Object.isFrozen(n3)) throw new Error("waitUntil can NOT be called asynchronous");
              s3 && (t4 = s3(t4, e4)), n3.push(t4);
            } });
            try {
              e4(o2);
            } catch (e5) {
              (0, i2.onUnexpectedError)(e5);
              continue;
            }
            Object.freeze(n3), yield Promise.allSettled(n3).then(((e5) => {
              for (const t4 of e5) "rejected" === t4.status && (0, i2.onUnexpectedError)(t4.reason);
            }));
          }
        });
      }
    };
    class b extends v {
      get isPaused() {
        return 0 !== this._isPaused;
      }
      constructor(e3) {
        super(e3), this._isPaused = 0, this._eventQueue = new o.LinkedList(), this._mergeFn = e3 == null ? void 0 : e3.merge;
      }
      pause() {
        this._isPaused++;
      }
      resume() {
        if (0 !== this._isPaused && 0 == --this._isPaused) if (this._mergeFn) {
          if (this._eventQueue.size > 0) {
            const e3 = Array.from(this._eventQueue);
            this._eventQueue.clear(), super.fire(this._mergeFn(e3));
          }
        } else for (; !this._isPaused && 0 !== this._eventQueue.size; ) super.fire(this._eventQueue.shift());
      }
      fire(e3) {
        this._size && (0 !== this._isPaused ? this._eventQueue.push(e3) : super.fire(e3));
      }
    }
    t2.PauseableEmitter = b, t2.DebounceEmitter = class extends b {
      constructor(e3) {
        var _a;
        super(e3), this._delay = (_a = e3.delay) != null ? _a : 100;
      }
      fire(e3) {
        this._handle || (this.pause(), this._handle = setTimeout((() => {
          this._handle = void 0, this.resume();
        }), this._delay)), super.fire(e3);
      }
    }, t2.MicrotaskEmitter = class extends v {
      constructor(e3) {
        super(e3), this._queuedEvents = [], this._mergeFn = e3 == null ? void 0 : e3.merge;
      }
      fire(e3) {
        this.hasListeners() && (this._queuedEvents.push(e3), 1 === this._queuedEvents.length && queueMicrotask((() => {
          this._mergeFn ? super.fire(this._mergeFn(this._queuedEvents)) : this._queuedEvents.forEach(((e4) => super.fire(e4))), this._queuedEvents = [];
        })));
      }
    };
    class S {
      constructor() {
        this.hasListeners = false, this.events = [], this.emitter = new v({ onWillAddFirstListener: () => this.onFirstListenerAdd(), onDidRemoveLastListener: () => this.onLastListenerRemove() });
      }
      get event() {
        return this.emitter.event;
      }
      add(e3) {
        const t3 = { event: e3, listener: null };
        return this.events.push(t3), this.hasListeners && this.hook(t3), (0, n2.toDisposable)((0, r2.createSingleCallFunction)((() => {
          this.hasListeners && this.unhook(t3);
          const e4 = this.events.indexOf(t3);
          this.events.splice(e4, 1);
        })));
      }
      onFirstListenerAdd() {
        this.hasListeners = true, this.events.forEach(((e3) => this.hook(e3)));
      }
      onLastListenerRemove() {
        this.hasListeners = false, this.events.forEach(((e3) => this.unhook(e3)));
      }
      hook(e3) {
        e3.listener = e3.event(((e4) => this.emitter.fire(e4)));
      }
      unhook(e3) {
        var _a;
        (_a = e3.listener) == null ? void 0 : _a.dispose(), e3.listener = null;
      }
      dispose() {
        var _a;
        this.emitter.dispose();
        for (const e3 of this.events) (_a = e3.listener) == null ? void 0 : _a.dispose();
        this.events = [];
      }
    }
    t2.EventMultiplexer = S, t2.DynamicListEventMultiplexer = class {
      constructor(e3, t3, s3, i3) {
        this._store = new n2.DisposableStore();
        const r3 = this._store.add(new S()), o2 = this._store.add(new n2.DisposableMap());
        function a2(e4) {
          o2.set(e4, r3.add(i3(e4)));
        }
        for (const t4 of e3) a2(t4);
        this._store.add(t3(((e4) => {
          a2(e4);
        }))), this._store.add(s3(((e4) => {
          o2.deleteAndDispose(e4);
        }))), this.event = r3.event;
      }
      dispose() {
        this._store.dispose();
      }
    }, t2.EventBufferer = class {
      constructor() {
        this.data = [];
      }
      wrapEvent(e3, t3, s3) {
        return (i3, r3, n3) => e3(((e4) => {
          var _a;
          const n4 = this.data[this.data.length - 1];
          if (!t3) return void (n4 ? n4.buffers.push((() => i3.call(r3, e4))) : i3.call(r3, e4));
          const o2 = n4;
          o2 ? ((_a = o2.items) != null ? _a : o2.items = [], o2.items.push(e4), 0 === o2.buffers.length && n4.buffers.push((() => {
            var _a2;
            (_a2 = o2.reducedResult) != null ? _a2 : o2.reducedResult = s3 ? o2.items.reduce(t3, s3) : o2.items.reduce(t3), i3.call(r3, o2.reducedResult);
          }))) : i3.call(r3, t3(s3, e4));
        }), void 0, n3);
      }
      bufferEvents(e3) {
        const t3 = { buffers: new Array() };
        this.data.push(t3);
        const s3 = e3();
        return this.data.pop(), t3.buffers.forEach(((e4) => e4())), s3;
      }
    }, t2.Relay = class {
      constructor() {
        this.listening = false, this.inputEvent = h.None, this.inputEventListener = n2.Disposable.None, this.emitter = new v({ onDidAddFirstListener: () => {
          this.listening = true, this.inputEventListener = this.inputEvent(this.emitter.fire, this.emitter);
        }, onDidRemoveLastListener: () => {
          this.listening = false, this.inputEventListener.dispose();
        } }), this.event = this.emitter.event;
      }
      set input(e3) {
        this.inputEvent = e3, this.listening && (this.inputEventListener.dispose(), this.inputEventListener = e3(this.emitter.fire, this.emitter));
      }
      dispose() {
        this.inputEventListener.dispose(), this.emitter.dispose();
      }
    }, t2.ValueWithChangeEvent = class {
      static const(e3) {
        return new y(e3);
      }
      constructor(e3) {
        this._value = e3, this._onDidChange = new v(), this.onDidChange = this._onDidChange.event;
      }
      get value() {
        return this._value;
      }
      set value(e3) {
        e3 !== this._value && (this._value = e3, this._onDidChange.fire(void 0));
      }
    };
    class y {
      constructor(e3) {
        this.value = e3, this.onDidChange = h.None;
      }
    }
  }, 8841: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.createSingleCallFunction = function(e3, t3) {
      const s2 = this;
      let i2, r2 = false;
      return function() {
        if (r2) return i2;
        if (r2 = true, t3) try {
          i2 = e3.apply(s2, arguments);
        } finally {
          t3();
        }
        else i2 = e3.apply(s2, arguments);
        return i2;
      };
    };
  }, 4218: (e2, t2) => {
    var s2;
    Object.defineProperty(t2, "__esModule", { value: true }), t2.Iterable = void 0, (function(e3) {
      function t3(e4) {
        return e4 && "object" == typeof e4 && "function" == typeof e4[Symbol.iterator];
      }
      e3.is = t3;
      const s3 = Object.freeze([]);
      function* i2(e4) {
        yield e4;
      }
      e3.empty = function() {
        return s3;
      }, e3.single = i2, e3.wrap = function(e4) {
        return t3(e4) ? e4 : i2(e4);
      }, e3.from = function(e4) {
        return e4 || s3;
      }, e3.reverse = function* (e4) {
        for (let t4 = e4.length - 1; t4 >= 0; t4--) yield e4[t4];
      }, e3.isEmpty = function(e4) {
        return !e4 || true === e4[Symbol.iterator]().next().done;
      }, e3.first = function(e4) {
        return e4[Symbol.iterator]().next().value;
      }, e3.some = function(e4, t4) {
        let s4 = 0;
        for (const i3 of e4) if (t4(i3, s4++)) return true;
        return false;
      }, e3.find = function(e4, t4) {
        for (const s4 of e4) if (t4(s4)) return s4;
      }, e3.filter = function* (e4, t4) {
        for (const s4 of e4) t4(s4) && (yield s4);
      }, e3.map = function* (e4, t4) {
        let s4 = 0;
        for (const i3 of e4) yield t4(i3, s4++);
      }, e3.flatMap = function* (e4, t4) {
        let s4 = 0;
        for (const i3 of e4) yield* __yieldStar(t4(i3, s4++));
      }, e3.concat = function* (...e4) {
        for (const t4 of e4) yield* __yieldStar(t4);
      }, e3.reduce = function(e4, t4, s4) {
        let i3 = s4;
        for (const s5 of e4) i3 = t4(i3, s5);
        return i3;
      }, e3.slice = function* (e4, t4, s4 = e4.length) {
        for (t4 < 0 && (t4 += e4.length), s4 < 0 ? s4 += e4.length : s4 > e4.length && (s4 = e4.length); t4 < s4; t4++) yield e4[t4];
      }, e3.consume = function(t4, s4 = Number.POSITIVE_INFINITY) {
        const i3 = [];
        if (0 === s4) return [i3, t4];
        const r2 = t4[Symbol.iterator]();
        for (let t5 = 0; t5 < s4; t5++) {
          const t6 = r2.next();
          if (t6.done) return [i3, e3.empty()];
          i3.push(t6.value);
        }
        return [i3, { [Symbol.iterator]: () => r2 }];
      }, e3.asyncToArray = function(e4) {
        return __async(this, null, function* () {
          const t4 = [];
          try {
            for (var iter = __forAwait(e4), more, temp, error; more = !(temp = yield iter.next()).done; more = false) {
              const s4 = temp.value;
              t4.push(s4);
            }
          } catch (temp) {
            error = [temp];
          } finally {
            try {
              more && (temp = iter.return) && (yield temp.call(iter));
            } finally {
              if (error)
                throw error[0];
            }
          }
          return Promise.resolve(t4);
        });
      };
    })(s2 || (t2.Iterable = s2 = {}));
  }, 7150: (e2, t2, s2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.DisposableMap = t2.ImmortalReference = t2.AsyncReferenceCollection = t2.ReferenceCollection = t2.SafeDisposable = t2.RefCountedDisposable = t2.MandatoryMutableDisposable = t2.MutableDisposable = t2.Disposable = t2.DisposableStore = t2.DisposableTracker = void 0, t2.setDisposableTracker = function(e3) {
      h = e3;
    }, t2.trackDisposable = l, t2.markAsDisposed = u, t2.markAsSingleton = function(e3) {
      return h == null ? void 0 : h.markAsSingleton(e3), e3;
    }, t2.isDisposable = f, t2.dispose = _, t2.disposeIfDisposable = function(e3) {
      for (const t3 of e3) f(t3) && t3.dispose();
      return [];
    }, t2.combinedDisposable = function(...e3) {
      const t3 = p((() => _(e3)));
      return (function(e4, t4) {
        if (h) for (const s3 of e4) h.setParent(s3, t4);
      })(e3, t3), t3;
    }, t2.toDisposable = p, t2.disposeOnReturn = function(e3) {
      const t3 = new g();
      try {
        e3(t3);
      } finally {
        t3.dispose();
      }
    };
    const i2 = s2(3058), r2 = s2(9087), n2 = s2(2608), o = s2(8841), a = s2(4218);
    let h = null;
    const _c = class _c {
      constructor() {
        this.livingDisposables = /* @__PURE__ */ new Map();
      }
      getDisposableData(e3) {
        let t3 = this.livingDisposables.get(e3);
        return t3 || (t3 = { parent: null, source: null, isSingleton: false, value: e3, idx: _c.idx++ }, this.livingDisposables.set(e3, t3)), t3;
      }
      trackDisposable(e3) {
        const t3 = this.getDisposableData(e3);
        t3.source || (t3.source = new Error().stack);
      }
      setParent(e3, t3) {
        this.getDisposableData(e3).parent = t3;
      }
      markAsDisposed(e3) {
        this.livingDisposables.delete(e3);
      }
      markAsSingleton(e3) {
        this.getDisposableData(e3).isSingleton = true;
      }
      getRootParent(e3, t3) {
        const s3 = t3.get(e3);
        if (s3) return s3;
        const i3 = e3.parent ? this.getRootParent(this.getDisposableData(e3.parent), t3) : e3;
        return t3.set(e3, i3), i3;
      }
      getTrackedDisposables() {
        const e3 = /* @__PURE__ */ new Map();
        return [...this.livingDisposables.entries()].filter((([, t3]) => null !== t3.source && !this.getRootParent(t3, e3).isSingleton)).flatMap((([e4]) => e4));
      }
      computeLeakingDisposables(e3 = 10, t3) {
        let s3;
        if (t3) s3 = t3;
        else {
          const e4 = /* @__PURE__ */ new Map(), t4 = [...this.livingDisposables.values()].filter(((t5) => null !== t5.source && !this.getRootParent(t5, e4).isSingleton));
          if (0 === t4.length) return;
          const i3 = new Set(t4.map(((e5) => e5.value)));
          if (s3 = t4.filter(((e5) => !(e5.parent && i3.has(e5.parent)))), 0 === s3.length) throw new Error("There are cyclic diposable chains!");
        }
        if (!s3) return;
        function o2(e4) {
          const t4 = e4.source.split("\n").map(((e5) => e5.trim().replace("at ", ""))).filter(((e5) => "" !== e5));
          return (function(e5, t5) {
            for (; e5.length > 0 && t5.some(((t6) => "string" == typeof t6 ? t6 === e5[0] : e5[0].match(t6))); ) e5.shift();
          })(t4, ["Error", /^trackDisposable \(.*\)$/, /^DisposableTracker.trackDisposable \(.*\)$/]), t4.reverse();
        }
        const a2 = new n2.SetMap();
        for (const e4 of s3) {
          const t4 = o2(e4);
          for (let s4 = 0; s4 <= t4.length; s4++) a2.add(t4.slice(0, s4).join("\n"), e4);
        }
        s3.sort((0, i2.compareBy)(((e4) => e4.idx), i2.numberComparator));
        let h2 = "", c2 = 0;
        for (const t4 of s3.slice(0, e3)) {
          c2++;
          const e4 = o2(t4), i3 = [];
          for (let t5 = 0; t5 < e4.length; t5++) {
            let n3 = e4[t5];
            n3 = `(shared with ${a2.get(e4.slice(0, t5 + 1).join("\n")).size}/${s3.length} leaks) at ${n3}`;
            const h3 = a2.get(e4.slice(0, t5).join("\n")), c3 = (0, r2.groupBy)([...h3].map(((e5) => o2(e5)[t5])), ((e5) => e5));
            delete c3[e4[t5]];
            for (const [e5, t6] of Object.entries(c3)) i3.unshift(`    - stacktraces of ${t6.length} other leaks continue with ${e5}`);
            i3.unshift(n3);
          }
          h2 += `


==================== Leaking disposable ${c2}/${s3.length}: ${t4.value.constructor.name} ====================
${i3.join("\n")}
============================================================

`;
        }
        return s3.length > e3 && (h2 += `


... and ${s3.length - e3} more leaking disposables

`), { leaks: s3, details: h2 };
      }
    };
    _c.idx = 0;
    let c = _c;
    function l(e3) {
      return h == null ? void 0 : h.trackDisposable(e3), e3;
    }
    function u(e3) {
      h == null ? void 0 : h.markAsDisposed(e3);
    }
    function d(e3, t3) {
      h == null ? void 0 : h.setParent(e3, t3);
    }
    function f(e3) {
      return "object" == typeof e3 && null !== e3 && "function" == typeof e3.dispose && 0 === e3.dispose.length;
    }
    function _(e3) {
      if (a.Iterable.is(e3)) {
        const t3 = [];
        for (const s3 of e3) if (s3) try {
          s3.dispose();
        } catch (e4) {
          t3.push(e4);
        }
        if (1 === t3.length) throw t3[0];
        if (t3.length > 1) throw new AggregateError(t3, "Encountered errors while disposing of store");
        return Array.isArray(e3) ? [] : e3;
      }
      if (e3) return e3.dispose(), e3;
    }
    function p(e3) {
      const t3 = l({ dispose: (0, o.createSingleCallFunction)((() => {
        u(t3), e3();
      })) });
      return t3;
    }
    t2.DisposableTracker = c;
    const _g = class _g {
      constructor() {
        this._toDispose = /* @__PURE__ */ new Set(), this._isDisposed = false, l(this);
      }
      dispose() {
        this._isDisposed || (u(this), this._isDisposed = true, this.clear());
      }
      get isDisposed() {
        return this._isDisposed;
      }
      clear() {
        if (0 !== this._toDispose.size) try {
          _(this._toDispose);
        } finally {
          this._toDispose.clear();
        }
      }
      add(e3) {
        if (!e3) return e3;
        if (e3 === this) throw new Error("Cannot register a disposable on itself!");
        return d(e3, this), this._isDisposed ? _g.DISABLE_DISPOSED_WARNING || console.warn(new Error("Trying to add a disposable to a DisposableStore that has already been disposed of. The added object will be leaked!").stack) : this._toDispose.add(e3), e3;
      }
      delete(e3) {
        if (e3) {
          if (e3 === this) throw new Error("Cannot dispose a disposable on itself!");
          this._toDispose.delete(e3), e3.dispose();
        }
      }
      deleteAndLeak(e3) {
        e3 && this._toDispose.has(e3) && (this._toDispose.delete(e3), d(e3, null));
      }
    };
    _g.DISABLE_DISPOSED_WARNING = false;
    let g = _g;
    t2.DisposableStore = g;
    const _v = class _v {
      constructor() {
        this._store = new g(), l(this), d(this._store, this);
      }
      dispose() {
        u(this), this._store.dispose();
      }
      _register(e3) {
        if (e3 === this) throw new Error("Cannot register a disposable on itself!");
        return this._store.add(e3);
      }
    };
    _v.None = Object.freeze({ dispose() {
    } });
    let v = _v;
    t2.Disposable = v;
    class m {
      constructor() {
        this._isDisposed = false, l(this);
      }
      get value() {
        return this._isDisposed ? void 0 : this._value;
      }
      set value(e3) {
        var _a;
        this._isDisposed || e3 === this._value || ((_a = this._value) == null ? void 0 : _a.dispose(), e3 && d(e3, this), this._value = e3);
      }
      clear() {
        this.value = void 0;
      }
      dispose() {
        var _a;
        this._isDisposed = true, u(this), (_a = this._value) == null ? void 0 : _a.dispose(), this._value = void 0;
      }
      clearAndLeak() {
        const e3 = this._value;
        return this._value = void 0, e3 && d(e3, null), e3;
      }
    }
    t2.MutableDisposable = m, t2.MandatoryMutableDisposable = class {
      constructor(e3) {
        this._disposable = new m(), this._isDisposed = false, this._disposable.value = e3;
      }
      get value() {
        return this._disposable.value;
      }
      set value(e3) {
        this._isDisposed || e3 === this._disposable.value || (this._disposable.value = e3);
      }
      dispose() {
        this._isDisposed = true, this._disposable.dispose();
      }
    }, t2.RefCountedDisposable = class {
      constructor(e3) {
        this._disposable = e3, this._counter = 1;
      }
      acquire() {
        return this._counter++, this;
      }
      release() {
        return 0 == --this._counter && this._disposable.dispose(), this;
      }
    }, t2.SafeDisposable = class {
      constructor() {
        this.dispose = () => {
        }, this.unset = () => {
        }, this.isset = () => false, l(this);
      }
      set(e3) {
        let t3 = e3;
        return this.unset = () => t3 = void 0, this.isset = () => void 0 !== t3, this.dispose = () => {
          t3 && (t3(), t3 = void 0, u(this));
        }, this;
      }
    }, t2.ReferenceCollection = class {
      constructor() {
        this.references = /* @__PURE__ */ new Map();
      }
      acquire(e3, ...t3) {
        let s3 = this.references.get(e3);
        s3 || (s3 = { counter: 0, object: this.createReferencedObject(e3, ...t3) }, this.references.set(e3, s3));
        const { object: i3 } = s3, r3 = (0, o.createSingleCallFunction)((() => {
          0 == --s3.counter && (this.destroyReferencedObject(e3, s3.object), this.references.delete(e3));
        }));
        return s3.counter++, { object: i3, dispose: r3 };
      }
    }, t2.AsyncReferenceCollection = class {
      constructor(e3) {
        this.referenceCollection = e3;
      }
      acquire(e3, ...t3) {
        return __async(this, null, function* () {
          const s3 = this.referenceCollection.acquire(e3, ...t3);
          try {
            return { object: yield s3.object, dispose: () => s3.dispose() };
          } catch (e4) {
            throw s3.dispose(), e4;
          }
        });
      }
    }, t2.ImmortalReference = class {
      constructor(e3) {
        this.object = e3;
      }
      dispose() {
      }
    };
    class b {
      constructor() {
        this._store = /* @__PURE__ */ new Map(), this._isDisposed = false, l(this);
      }
      dispose() {
        u(this), this._isDisposed = true, this.clearAndDisposeAll();
      }
      clearAndDisposeAll() {
        if (this._store.size) try {
          _(this._store.values());
        } finally {
          this._store.clear();
        }
      }
      has(e3) {
        return this._store.has(e3);
      }
      get size() {
        return this._store.size;
      }
      get(e3) {
        return this._store.get(e3);
      }
      set(e3, t3, s3 = false) {
        var _a;
        this._isDisposed && console.warn(new Error("Trying to add a disposable to a DisposableMap that has already been disposed of. The added object will be leaked!").stack), s3 || ((_a = this._store.get(e3)) == null ? void 0 : _a.dispose()), this._store.set(e3, t3);
      }
      deleteAndDispose(e3) {
        var _a;
        (_a = this._store.get(e3)) == null ? void 0 : _a.dispose(), this._store.delete(e3);
      }
      deleteAndLeak(e3) {
        const t3 = this._store.get(e3);
        return this._store.delete(e3), t3;
      }
      keys() {
        return this._store.keys();
      }
      values() {
        return this._store.values();
      }
      [Symbol.iterator]() {
        return this._store[Symbol.iterator]();
      }
    }
    t2.DisposableMap = b;
  }, 6317: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.LinkedList = void 0;
    const _s = class _s {
      constructor(e3) {
        this.element = e3, this.next = _s.Undefined, this.prev = _s.Undefined;
      }
    };
    _s.Undefined = new _s(void 0);
    let s2 = _s;
    class i2 {
      constructor() {
        this._first = s2.Undefined, this._last = s2.Undefined, this._size = 0;
      }
      get size() {
        return this._size;
      }
      isEmpty() {
        return this._first === s2.Undefined;
      }
      clear() {
        let e3 = this._first;
        for (; e3 !== s2.Undefined; ) {
          const t3 = e3.next;
          e3.prev = s2.Undefined, e3.next = s2.Undefined, e3 = t3;
        }
        this._first = s2.Undefined, this._last = s2.Undefined, this._size = 0;
      }
      unshift(e3) {
        return this._insert(e3, false);
      }
      push(e3) {
        return this._insert(e3, true);
      }
      _insert(e3, t3) {
        const i3 = new s2(e3);
        if (this._first === s2.Undefined) this._first = i3, this._last = i3;
        else if (t3) {
          const e4 = this._last;
          this._last = i3, i3.prev = e4, e4.next = i3;
        } else {
          const e4 = this._first;
          this._first = i3, i3.next = e4, e4.prev = i3;
        }
        this._size += 1;
        let r2 = false;
        return () => {
          r2 || (r2 = true, this._remove(i3));
        };
      }
      shift() {
        if (this._first !== s2.Undefined) {
          const e3 = this._first.element;
          return this._remove(this._first), e3;
        }
      }
      pop() {
        if (this._last !== s2.Undefined) {
          const e3 = this._last.element;
          return this._remove(this._last), e3;
        }
      }
      _remove(e3) {
        if (e3.prev !== s2.Undefined && e3.next !== s2.Undefined) {
          const t3 = e3.prev;
          t3.next = e3.next, e3.next.prev = t3;
        } else e3.prev === s2.Undefined && e3.next === s2.Undefined ? (this._first = s2.Undefined, this._last = s2.Undefined) : e3.next === s2.Undefined ? (this._last = this._last.prev, this._last.next = s2.Undefined) : e3.prev === s2.Undefined && (this._first = this._first.next, this._first.prev = s2.Undefined);
        this._size -= 1;
      }
      *[Symbol.iterator]() {
        let e3 = this._first;
        for (; e3 !== s2.Undefined; ) yield e3.element, e3 = e3.next;
      }
    }
    t2.LinkedList = i2;
  }, 2608: (e2, t2) => {
    var s2;
    Object.defineProperty(t2, "__esModule", { value: true }), t2.SetMap = t2.BidirectionalMap = t2.CounterSet = t2.Touch = void 0, t2.getOrSet = function(e3, t3, s3) {
      let i2 = e3.get(t3);
      return void 0 === i2 && (i2 = s3, e3.set(t3, i2)), i2;
    }, t2.mapToString = function(e3) {
      const t3 = [];
      return e3.forEach(((e4, s3) => {
        t3.push(`${s3} => ${e4}`);
      })), `Map(${e3.size}) {${t3.join(", ")}}`;
    }, t2.setToString = function(e3) {
      const t3 = [];
      return e3.forEach(((e4) => {
        t3.push(e4);
      })), `Set(${e3.size}) {${t3.join(", ")}}`;
    }, t2.mapsStrictEqualIgnoreOrder = function(e3, t3) {
      if (e3 === t3) return true;
      if (e3.size !== t3.size) return false;
      for (const [s3, i2] of e3) if (!t3.has(s3) || t3.get(s3) !== i2) return false;
      for (const [s3] of t3) if (!e3.has(s3)) return false;
      return true;
    }, (function(e3) {
      e3[e3.None = 0] = "None", e3[e3.AsOld = 1] = "AsOld", e3[e3.AsNew = 2] = "AsNew";
    })(s2 || (t2.Touch = s2 = {})), t2.CounterSet = class {
      constructor() {
        this.map = /* @__PURE__ */ new Map();
      }
      add(e3) {
        return this.map.set(e3, (this.map.get(e3) || 0) + 1), this;
      }
      delete(e3) {
        let t3 = this.map.get(e3) || 0;
        return 0 !== t3 && (t3--, 0 === t3 ? this.map.delete(e3) : this.map.set(e3, t3), true);
      }
      has(e3) {
        return this.map.has(e3);
      }
    }, t2.BidirectionalMap = class {
      constructor(e3) {
        if (this._m1 = /* @__PURE__ */ new Map(), this._m2 = /* @__PURE__ */ new Map(), e3) for (const [t3, s3] of e3) this.set(t3, s3);
      }
      clear() {
        this._m1.clear(), this._m2.clear();
      }
      set(e3, t3) {
        this._m1.set(e3, t3), this._m2.set(t3, e3);
      }
      get(e3) {
        return this._m1.get(e3);
      }
      getKey(e3) {
        return this._m2.get(e3);
      }
      delete(e3) {
        const t3 = this._m1.get(e3);
        return void 0 !== t3 && (this._m1.delete(e3), this._m2.delete(t3), true);
      }
      forEach(e3, t3) {
        this._m1.forEach(((s3, i2) => {
          e3.call(t3, s3, i2, this);
        }));
      }
      keys() {
        return this._m1.keys();
      }
      values() {
        return this._m1.values();
      }
    }, t2.SetMap = class {
      constructor() {
        this.map = /* @__PURE__ */ new Map();
      }
      add(e3, t3) {
        let s3 = this.map.get(e3);
        s3 || (s3 = /* @__PURE__ */ new Set(), this.map.set(e3, s3)), s3.add(t3);
      }
      delete(e3, t3) {
        const s3 = this.map.get(e3);
        s3 && (s3.delete(t3), 0 === s3.size && this.map.delete(e3));
      }
      forEach(e3, t3) {
        const s3 = this.map.get(e3);
        s3 && s3.forEach(t3);
      }
      get(e3) {
        return this.map.get(e3) || /* @__PURE__ */ new Set();
      }
    };
  }, 9725: (e2, t2) => {
    Object.defineProperty(t2, "__esModule", { value: true }), t2.StopWatch = void 0;
    const s2 = globalThis.performance && "function" == typeof globalThis.performance.now;
    class i2 {
      static create(e3) {
        return new i2(e3);
      }
      constructor(e3) {
        this._now = s2 && false === e3 ? Date.now : globalThis.performance.now.bind(globalThis.performance), this._startTime = this._now(), this._stopTime = -1;
      }
      stop() {
        this._stopTime = this._now();
      }
      reset() {
        this._startTime = this._now(), this._stopTime = -1;
      }
      elapsed() {
        return -1 !== this._stopTime ? this._stopTime - this._startTime : this._now() - this._startTime;
      }
    }
    t2.StopWatch = i2;
  } }, t = {};
  function s(i2) {
    var r2 = t[i2];
    if (void 0 !== r2) return r2.exports;
    var n2 = t[i2] = { exports: {} };
    return e[i2].call(n2.exports, n2, n2.exports, s), n2.exports;
  }
  var i = {};
  (() => {
    var e2 = i;
    Object.defineProperty(e2, "__esModule", { value: true }), e2.Terminal = void 0;
    const t2 = s(5101), r2 = s(6097), n2 = s(4335), o = s(5856), a = s(3027), h = s(7150), c = ["cols", "rows"];
    class l extends h.Disposable {
      constructor(e3) {
        super(), this._core = this._register(new o.Terminal(e3)), this._addonManager = this._register(new a.AddonManager()), this._publicOptions = __spreadValues({}, this._core.options);
        const t3 = (e4) => this._core.options[e4], s2 = (e4, t4) => {
          this._checkReadonlyOptions(e4), this._core.options[e4] = t4;
        };
        for (const e4 in this._core.options) {
          Object.defineProperty(this._publicOptions, e4, { get: () => this._core.options[e4], set: (t4) => {
            this._checkReadonlyOptions(e4), this._core.options[e4] = t4;
          } });
          const i2 = { get: t3.bind(this, e4), set: s2.bind(this, e4) };
          Object.defineProperty(this._publicOptions, e4, i2);
        }
      }
      _checkReadonlyOptions(e3) {
        if (c.includes(e3)) throw new Error(`Option "${e3}" can only be set in the constructor`);
      }
      _checkProposedApi() {
        if (!this._core.optionsService.options.allowProposedApi) throw new Error("You must set the allowProposedApi option to true to use proposed API");
      }
      get onBell() {
        return this._core.onBell;
      }
      get onBinary() {
        return this._core.onBinary;
      }
      get onCursorMove() {
        return this._core.onCursorMove;
      }
      get onData() {
        return this._core.onData;
      }
      get onLineFeed() {
        return this._core.onLineFeed;
      }
      get onResize() {
        return this._core.onResize;
      }
      get onScroll() {
        return this._core.onScroll;
      }
      get onTitleChange() {
        return this._core.onTitleChange;
      }
      get onWriteParsed() {
        return this._core.onWriteParsed;
      }
      get parser() {
        return this._checkProposedApi(), this._parser || (this._parser = new r2.ParserApi(this._core)), this._parser;
      }
      get unicode() {
        return this._checkProposedApi(), new n2.UnicodeApi(this._core);
      }
      get rows() {
        return this._core.rows;
      }
      get cols() {
        return this._core.cols;
      }
      get buffer() {
        return this._checkProposedApi(), this._buffer || (this._buffer = this._register(new t2.BufferNamespaceApi(this._core))), this._buffer;
      }
      get markers() {
        return this._checkProposedApi(), this._core.markers;
      }
      get modes() {
        const e3 = this._core.coreService.decPrivateModes;
        let t3 = "none";
        switch (this._core.coreMouseService.activeProtocol) {
          case "X10":
            t3 = "x10";
            break;
          case "VT200":
            t3 = "vt200";
            break;
          case "DRAG":
            t3 = "drag";
            break;
          case "ANY":
            t3 = "any";
        }
        return { applicationCursorKeysMode: e3.applicationCursorKeys, applicationKeypadMode: e3.applicationKeypad, bracketedPasteMode: e3.bracketedPasteMode, insertMode: this._core.coreService.modes.insertMode, mouseTrackingMode: t3, originMode: e3.origin, reverseWraparoundMode: e3.reverseWraparound, sendFocusMode: e3.sendFocus, synchronizedOutputMode: e3.synchronizedOutput, wraparoundMode: e3.wraparound };
      }
      get options() {
        return this._publicOptions;
      }
      set options(e3) {
        for (const t3 in e3) this._publicOptions[t3] = e3[t3];
      }
      input(e3, t3 = true) {
        this._core.input(e3, t3);
      }
      resize(e3, t3) {
        this._verifyIntegers(e3, t3), this._core.resize(e3, t3);
      }
      registerMarker(e3 = 0) {
        return this._checkProposedApi(), this._verifyIntegers(e3), this._core.addMarker(e3);
      }
      addMarker(e3) {
        return this.registerMarker(e3);
      }
      dispose() {
        super.dispose();
      }
      scrollLines(e3) {
        this._verifyIntegers(e3), this._core.scrollLines(e3);
      }
      scrollPages(e3) {
        this._verifyIntegers(e3), this._core.scrollPages(e3);
      }
      scrollToTop() {
        this._core.scrollToTop();
      }
      scrollToBottom() {
        this._core.scrollToBottom();
      }
      scrollToLine(e3) {
        this._verifyIntegers(e3), this._core.scrollToLine(e3);
      }
      clear() {
        this._core.clear();
      }
      write(e3, t3) {
        this._core.write(e3, t3);
      }
      writeln(e3, t3) {
        this._core.write(e3), this._core.write("\r\n", t3);
      }
      reset() {
        this._core.reset();
      }
      loadAddon(e3) {
        this._addonManager.loadAddon(this, e3);
      }
      _verifyIntegers(...e3) {
        for (const t3 of e3) if (t3 === 1 / 0 || isNaN(t3) || t3 % 1 != 0) throw new Error("This API only accepts integers");
      }
    }
    e2.Terminal = l;
  })();
  var r = exports;
  for (var n in i) r[n] = i[n];
  i.__esModule && Object.defineProperty(r, "__esModule", { value: true });
})();
